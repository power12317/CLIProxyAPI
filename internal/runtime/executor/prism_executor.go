package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	prismModel            = "gpt-5.6-sol"
	prismMaxResponseBytes = 2 << 20
)

// PrismExecutor is the P0/P1 local browser-adapter executor. It deliberately
// buffers the adapter response because Prism does not provide an authoritative
// token stream or usage object.
type PrismExecutor struct {
	cfg    *config.Config
	native *CodexExecutor
}

func NewPrismExecutor(cfg *config.Config) *PrismExecutor {
	return &PrismExecutor{cfg: cfg, native: NewCodexExecutor(cfg)}
}

func (e *PrismExecutor) Identifier() string { return "codex" }

func (e *PrismExecutor) enabled(auth *coreauth.Auth) bool {
	if e == nil || e.cfg == nil || !e.cfg.Codex.Prism.Enabled || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || auth.AuthKind() != coreauth.AuthKindOAuth || coreauth.IsPluginVirtualAuth(auth) {
		return false
	}
	return !auth.PrismBrowserExplicitlyDisabled()
}

func (e *PrismExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if e == nil || e.native == nil {
		return nil
	}
	return e.native.PrepareRequest(req, auth)
}

func (e *PrismExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.native == nil {
		return nil, errors.New("prism executor is unavailable")
	}
	return e.native.HttpRequest(ctx, auth, req)
}

func (e *PrismExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e == nil || e.native == nil {
		return nil, errors.New("prism executor is unavailable")
	}
	return e.native.Refresh(ctx, auth)
}

func (e *PrismExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, prismRequestError(http.StatusBadRequest, "unsupported_request", "Prism does not support token counting")
}

func (e *PrismExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, prismModel, auth)
	if err := e.validate(auth, req, opts); err != nil {
		reporter.PublishFailure(ctx, err)
		return coreexecutor.Response{}, err
	}
	body, headers, status, err := e.call(ctx, auth, req.Payload, false)
	if err != nil {
		reporter.PublishFailure(ctx, err)
		return coreexecutor.Response{}, err
	}
	if status != http.StatusOK {
		errStatus := prismAdapterError(status, body)
		reporter.PublishFailure(ctx, errStatus)
		return coreexecutor.Response{}, errStatus
	}
	body, err = ensurePrismUsageNull(body)
	if err != nil {
		reporter.PublishFailure(ctx, err)
		return coreexecutor.Response{}, err
	}
	reporter.EnsurePublished(ctx)
	return coreexecutor.Response{Payload: body, Headers: headers}, nil
}

func (e *PrismExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, prismModel, auth)
	if err := e.validate(auth, req, opts); err != nil {
		reporter.PublishFailure(ctx, err)
		return nil, err
	}
	body, headers, status, err := e.call(ctx, auth, req.Payload, true)
	if err != nil {
		reporter.PublishFailure(ctx, err)
		return nil, err
	}
	if status != http.StatusOK {
		errStatus := prismAdapterError(status, body)
		reporter.PublishFailure(ctx, errStatus)
		return nil, errStatus
	}
	if errValidate := validatePrismStream(body); errValidate != nil {
		reporter.PublishFailure(ctx, errValidate)
		return nil, errValidate
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: body}
	close(chunks)
	reporter.EnsurePublished(ctx)
	return &coreexecutor.StreamResult{Headers: headers, Chunks: chunks}, nil
}

func validatePrismStream(body []byte) error {
	if len(body) == 0 {
		return prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned an empty stream")
	}
	completed := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(data) {
			return prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned invalid SSE JSON")
		}
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.created":
		case "response.completed":
			if completed || gjson.GetBytes(data, "response.id").String() == "" || gjson.GetBytes(data, "response.model").String() != prismModel || gjson.GetBytes(data, "response.status").String() != "completed" {
				return prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned an invalid terminal response")
			}
			completed = true
		default:
			return prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned an unsupported SSE event")
		}
	}
	if !completed {
		return prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned no terminal response")
	}
	return nil
}

func (e *PrismExecutor) validate(auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) error {
	if !e.enabled(auth) {
		return prismRequestError(http.StatusBadRequest, "prism_disabled", "Prism is not enabled for this Codex OAuth account")
	}
	if opts.Alt != "" || (opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatOpenAIResponse && opts.SourceFormat != sdktranslator.FormatCodex) {
		return prismRequestError(http.StatusBadRequest, "unsupported_endpoint", "Prism supports only the Responses endpoint")
	}
	if len(req.Payload) == 0 || !gjson.ValidBytes(req.Payload) {
		return prismRequestError(http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
	}
	if strings.TrimSpace(req.Model) != prismModel || strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String()) != prismModel {
		return prismRequestError(http.StatusUnprocessableEntity, "unsupported_model", "Prism currently supports gpt-5.6-sol only")
	}
	if gjson.GetBytes(req.Payload, "reasoning.effort").Exists() && gjson.GetBytes(req.Payload, "reasoning.effort").String() != "medium" {
		return prismRequestError(http.StatusUnprocessableEntity, "unsupported_reasoning", "Prism currently supports medium reasoning only")
	}
	for _, path := range []string{"max_output_tokens", "temperature", "top_p", "background", "store", "include", "service_tier", "response_format"} {
		if gjson.GetBytes(req.Payload, path).Exists() {
			return prismRequestError(http.StatusUnprocessableEntity, "unsupported_request", "Prism P1 supports plain text Responses options only")
		}
	}
	if summary := gjson.GetBytes(req.Payload, "reasoning.summary"); summary.Exists() && summary.Type != gjson.Null && summary.String() != "none" {
		return prismRequestError(http.StatusUnprocessableEntity, "unsupported_reasoning", "Prism P1 does not support reasoning summaries")
	}
	if gjson.GetBytes(req.Payload, "tools").Exists() || gjson.GetBytes(req.Payload, "additional_tools").Exists() || gjson.GetBytes(req.Payload, "previous_response_id").Exists() || gjson.GetBytes(req.Payload, "conversation").Exists() {
		return prismRequestError(http.StatusUnprocessableEntity, "unsupported_request", "Prism P1 does not support tools or server-side conversation state")
	}
	input := gjson.GetBytes(req.Payload, "input")
	if !input.Exists() || input.Type == gjson.Null || (input.Type != gjson.String && !input.IsArray()) {
		return prismRequestError(http.StatusBadRequest, "invalid_request", "input must contain text")
	}
	if input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").Exists() && item.Get("type").String() != "message" {
				return prismRequestError(http.StatusUnprocessableEntity, "unsupported_input", "Prism P1 accepts text messages only")
			}
			content := item.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					partType := part.Get("type").String()
					if partType != "input_text" && partType != "output_text" && partType != "text" {
						return prismRequestError(http.StatusUnprocessableEntity, "unsupported_input", "Prism P1 accepts text messages only")
					}
				}
			}
		}
	}
	return nil
}

func (e *PrismExecutor) call(ctx context.Context, auth *coreauth.Auth, payload []byte, stream bool) ([]byte, http.Header, int, error) {
	endpoint, err := prismEndpoint(e.cfg.Codex.Prism.AdapterURL)
	if err != nil {
		return nil, nil, 0, err
	}
	key := strings.TrimSpace(os.Getenv("PRISM_ADAPTER_API_KEY"))
	if key == "" {
		return nil, nil, 0, prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter key is not configured")
	}
	token, _ := codexCreds(auth)
	accountID := helps.CodexOAuthAccountID(auth)
	if token == "" || accountID == "" {
		return nil, nil, 0, prismRequestError(http.StatusUnauthorized, "authentication_error", "Prism requires a Codex OAuth access token and account ID")
	}
	body := bytes.Clone(payload)
	var errSet error
	body, errSet = sjson.SetBytes(body, "stream", stream)
	if errSet != nil {
		return nil, nil, 0, errSet
	}
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errNew != nil {
		return nil, nil, 0, errNew
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Prism-Account-ID", accountID)
	req.Header.Set("X-Prism-OAuth-Token", token)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, nil, 0, prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter request failed; request was not replayed")
	}
	defer func() { _ = resp.Body.Close() }()
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, prismMaxResponseBytes+1))
	if errRead != nil || len(data) > prismMaxResponseBytes {
		return nil, nil, 0, prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter response exceeded the limit")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, nil, 0, prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter redirected unexpectedly")
	}
	return data, resp.Header.Clone(), resp.StatusCode, nil
}

func prismEndpoint(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "http://127.0.0.1:8319/v1"
	}
	parsed, err := url.Parse(base + "/responses")
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter must use a local HTTP endpoint")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || (!ip.IsLoopback()) {
		return "", prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter must bind to a numeric loopback address")
	}
	if parsed.Port() == "" {
		return "", prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter port is required")
	}
	return parsed.String(), nil
}

func ensurePrismUsageNull(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, prismRequestError(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned invalid JSON")
	}
	if gjson.GetBytes(body, "usage").Exists() {
		return sjson.SetBytes(body, "usage", nil)
	}
	return sjson.SetBytes(body, "usage", nil)
}

func prismAdapterError(status int, body []byte) error {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if code == "" {
		code = "prism_unavailable"
	}
	if message == "" {
		message = "Prism adapter returned an error"
	}
	return prismRequestError(status, code, message)
}

func prismRequestError(status int, code, message string) error {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"type": code, "message": message}})
	return statusErr{code: status, msg: string(payload), requestScoped: true}
}

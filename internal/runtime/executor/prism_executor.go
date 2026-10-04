package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const prismMaxResponseBytes = 2 << 20

// PrismExecutor is the local sub2api-compatible browser transport. It deliberately
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

func (e *PrismExecutor) routes(auth *coreauth.Auth, model string) bool {
	return e != nil && auth.UsesPrismForModel(e.cfg, model)
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

func (e *PrismExecutor) prepare(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) ([]byte, error) {
	if !e.routes(auth, req.Model) {
		return nil, prismRequestError(422, "unsupported_model", "Model is outside the Prism catalog")
	}
	if coreexecutor.DownstreamWebsocket(ctx) || coreexecutor.RequiredUpstreamWebsocket(ctx) || opts.Alt != "" || (opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatOpenAIResponse && opts.SourceFormat != sdktranslator.FormatCodex) {
		return nil, prismRequestError(400, "unsupported_endpoint", "Prism supports HTTP Responses only")
	}
	if !gjson.ValidBytes(req.Payload) || !gjson.ParseBytes(req.Payload).IsObject() {
		return nil, prismRequestError(400, "invalid_request", "request body must be a JSON object")
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	body, err := sjson.SetBytes(req.Payload, "model", model)
	if err != nil {
		return nil, err
	}
	// Use the canonical thinking pipeline for suffix precedence and translation.
	body, err = helps.ApplyRequestThinking(body, req, opts, opts.SourceFormat.String(), sdktranslator.FormatCodex.String(), "codex")
	if err != nil {
		return nil, err
	}
	if !gjson.GetBytes(body, "reasoning.effort").Exists() {
		body, err = sjson.SetBytes(body, "reasoning.effort", "medium")
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (e *PrismExecutor) execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, stream bool) (response coreexecutor.Response, err error) {
	model := thinking.ParseSuffix(req.Model).ModelName
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	reporter.SetStream(stream)
	defer func() { reporter.PublishUnavailable(ctx, err) }()
	payload, err := e.prepare(ctx, auth, req, opts)
	if err != nil {
		return response, err
	}
	reporter.SetTranslatedReasoningEffort(payload, sdktranslator.FormatCodex.String())
	body, headers, status, err := e.call(ctx, auth, req, opts, payload, stream)
	if err != nil {
		return response, err
	}
	if status != http.StatusOK {
		return response, helps.PrismAdapterError(status, body)
	}
	terminal, err := helps.PrismTerminal(body, model, stream, payload)
	if err != nil {
		return response, err
	}
	reporter.SetResponseModel(model)
	if stream {
		body = helps.PrismEvents(terminal)
	} else {
		body = terminal
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Del("Content-Length")
	headers.Set("X-Prism-Usage", "unavailable")
	headers.Set("X-Request-Id", gjson.GetBytes(terminal, "id").String())
	if stream {
		headers.Set("Content-Type", "text/event-stream")
	} else {
		headers.Set("Content-Type", "application/json")
	}
	return coreexecutor.Response{Payload: body, Headers: headers}, nil
}

func (e *PrismExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return e.execute(ctx, auth, req, opts, false)
}
func (e *PrismExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	response, err := e.execute(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: response.Headers, Chunks: chunks}, nil
}

func (e *PrismExecutor) call(ctx context.Context, auth *coreauth.Auth, original coreexecutor.Request, opts coreexecutor.Options, payload []byte, stream bool) ([]byte, http.Header, int, error) {
	endpoint, err := helps.PrismEndpoint()
	if err != nil {
		return nil, nil, 0, err
	}
	key := strings.TrimSpace(os.Getenv("PRISM_ADAPTER_API_KEY"))
	if key == "" {
		return nil, nil, 0, prismRequestError(http.StatusBadGateway, "prism_unavailable", "Prism adapter key is not configured")
	}
	token, _ := codexCreds(auth)
	accountID, callerID, sessionID := helps.PrismIdentity(auth, original, opts)
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
	if callerID != "" {
		req.Header.Set("X-Prism-Caller-ID", callerID)
	}
	if sessionID != "" {
		req.Header.Set("X-Prism-Session-ID", sessionID)
	}
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

func prismRequestError(status int, code, message string) error {
	return helps.NewPrismError(status, code, message)
}

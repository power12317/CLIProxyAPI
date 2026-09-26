package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// BasispointsExecutor shares the Codex credential lifecycle and changes only the
// upstream interface and its request/response protocol.
type BasispointsExecutor struct{ cfg *config.Config }

func NewBasispointsExecutor(cfg *config.Config) *BasispointsExecutor {
	return &BasispointsExecutor{cfg: cfg}
}
func (e *BasispointsExecutor) Identifier() string { return "codex" }

func (e *BasispointsExecutor) open(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter) (*http.Response, *basispoints.Bridge, []byte, error) {
	if opts.Alt != "" {
		return nil, nil, nil, statusErr{code: 400, msg: `{"error":{"type":"invalid_request_error","code":"unsupported_endpoint","message":"Basispoints uses the Responses endpoint; compact is not supported"}}`, requestScoped: true}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	original := req.Payload
	if len(opts.OriginalRequest) > 0 {
		original = opts.OriginalRequest
	}
	body := bytes.Clone(req.Payload)
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatOpenAIResponse
	}
	if from != sdktranslator.FormatOpenAIResponse && from != sdktranslator.FormatCodex {
		if !sdktranslator.HasRequestTransformer(from, sdktranslator.FormatCodex) {
			return nil, nil, nil, statusErr{code: 400, msg: `{"error":{"message":"unsupported Basispoints source protocol"}}`, requestScoped: true}
		}
		body = sdktranslator.TranslateRequest(from, sdktranslator.FormatCodex, baseModel, body, true)
	}
	var err error
	body, err = thinking.ApplyThinkingPassthrough(body, req.Payload, req.Model, from.String(), "basispoints")
	if err != nil {
		return nil, nil, nil, err
	}
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, nil, nil, err
	}
	body, err = sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, nil, nil, err
	}
	var authID, caller, session string
	if auth != nil {
		authID = auth.ID
	}
	if opts.Metadata != nil {
		caller, _ = opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
		session, _ = opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey].(string)
	}
	if session == "" {
		session = opts.Headers.Get("session_id")
	}
	if session == "" {
		session = opts.Headers.Get("X-Session-Id")
	}
	body, bridge, err := basispoints.Prepare(body, basispoints.Scope(caller, authID), session, &basispoints.SharedCache)
	if err != nil {
		helps.RecordBasispointsFailure(ctx, e.cfg, original, nil, nil, "prepare", err)
		return nil, nil, nil, err
	}
	body = helps.ApplyCodexFastMode(body, e.cfg)
	token, _ := codexCreds(auth)
	accountID := helps.CodexOAuthAccountID(auth)
	if accountID == "" {
		accountID = basispoints.AccountID(token)
	}
	if token == "" || accountID == "" {
		return nil, nil, nil, statusErr{code: 401, msg: `{"error":{"type":"authentication_error","message":"Basispoints requires a ChatGPT OAuth access token and account ID"}}`, requestScoped: true}
	}
	headers := basispoints.Headers(token, accountID)
	turnState := helps.NewBasispointsLogState(ctx, auth, basispoints.ResponsesURL, req.Payload, body, opts.Headers, baseModel)
	turnState.ObserveRequest(headers, nil)
	reporter.SetTranslatedReasoningEffort(body, "openai")
	reporter.SetCodexFastMode(e.cfg)
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = reporter.TrackHTTPClient(client)
	body, err = basispoints.UploadInputImages(ctx, client, body, headers, basispoints.Scope(caller, authID), &basispoints.SharedAttachments)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		helps.RecordBasispointsFailure(ctx, e.cfg, original, nil, nil, "attachment_upload", err)
		return nil, nil, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	httpReq.Header = headers
	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: httpReq.URL.String(), Method: httpReq.Method, Headers: httpReq.Header.Clone(), Body: body, Provider: "codex", AuthID: authID, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue})
	response, err := client.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone())
	turnState.ObserveResponse(response)
	turnState.LogResponse(ctx, e.cfg, false)
	bridge.ObserveEvent = func(raw []byte) {
		turnState.ObserveEvent(raw)
		if !strings.Contains(response.Header.Get("Content-Type"), "application/json") {
			helps.AppendAPIResponseChunk(ctx, e.cfg, append(append([]byte("data: "), raw...), '\n', '\n'))
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer func() {
			if errClose := response.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("basispoints: close error response")
			}
		}()
		raw, errRead := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		if errRead != nil {
			return nil, nil, nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
		helps.LogBasispointsRejection(ctx, response.StatusCode, body, response.Header)
		requestScoped := (&basispoints.Error{Status: response.StatusCode}).IsRequestScoped()
		errRejected := statusErr{code: response.StatusCode, msg: string(raw), requestScoped: requestScoped, retryAfter: openAICompatRetryAfter(response.StatusCode, response.Header, raw, time.Now())}
		if requestScoped {
			helps.RecordBasispointsFailure(ctx, e.cfg, original, body, raw, "upstream_rejected", errRejected)
		}
		return nil, nil, nil, errRejected
	}
	return response, bridge, body, nil
}

func (e *BasispointsExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result coreexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)
	response, bridge, body, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return result, err
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("basispoints: close response")
		}
	}()
	var completed []byte
	err = e.read(ctx, response, bridge, func(event []byte) error {
		if payload := basispoints.CompletedResponse(event); len(payload) > 0 {
			completed = payload
		}
		return nil
	})
	if err != nil {
		original := req.Payload
		if len(opts.OriginalRequest) > 0 {
			original = opts.OriginalRequest
		}
		helps.RecordBasispointsFailure(ctx, e.cfg, original, body, bridge.LastEvent, "response_conversion", err)
		return result, err
	}
	if len(completed) == 0 {
		return result, &basispoints.Error{Status: 502, Body: `{"error":{"message":"Basispoints returned no completed response"}}`}
	}
	e.publish(ctx, reporter, completed)
	format := coreexecutor.ResponseFormatOrSource(opts)
	if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex || format == "" {
		result.Payload = completed
	} else {
		wrapped, _ := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", completed)
		var param any
		result.Payload = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, format, req.Model, opts.OriginalRequest, body, wrapped, &param)
	}
	result.Headers = basispoints.ResponseHeaders(response.Header, false)
	return result, nil
}

func (e *BasispointsExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result *coreexecutor.StreamResult, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)
	response, bridge, body, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return nil, err
	}
	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := response.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("basispoints: close stream")
			}
		}()
		send := func(payload []byte) error {
			select {
			case out <- coreexecutor.StreamChunk{Payload: payload}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var completed []byte
		var param any
		format := coreexecutor.ResponseFormatOrSource(opts)
		errRead := e.read(ctx, response, bridge, func(event []byte) error {
			if payload := basispoints.CompletedResponse(event); len(payload) > 0 {
				completed = payload
			}
			if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex || format == "" {
				return send(event)
			}
			for _, line := range bytes.Split(event, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}
				for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, format, req.Model, opts.OriginalRequest, body, line, &param) {
					if errSend := send(chunk); errSend != nil {
						return errSend
					}
				}
			}
			return nil
		})
		if errRead != nil {
			original := req.Payload
			if len(opts.OriginalRequest) > 0 {
				original = opts.OriginalRequest
			}
			helps.RecordBasispointsFailure(ctx, e.cfg, original, body, bridge.LastEvent, "response_conversion", errRead)
			reporter.PublishFailure(ctx, errRead)
			select {
			case out <- coreexecutor.StreamChunk{Err: errRead}:
			case <-ctx.Done():
			}
			return
		}
		e.publish(ctx, reporter, completed)
	}()
	return &coreexecutor.StreamResult{Headers: basispoints.ResponseHeaders(response.Header, true), Chunks: out}, nil
}

func (e *BasispointsExecutor) read(ctx context.Context, response *http.Response, bridge *basispoints.Bridge, emit func([]byte) error) error {
	if strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
		if err != nil {
			return err
		}
		if len(raw) > 32<<20 {
			return fmt.Errorf("basispoints: response exceeds size limit")
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
		// Use the same event path for JSON responses so tool events are consistent.
		kind := "response.completed"
		if status := gjson.GetBytes(raw, "status").String(); status == "failed" || status == "incomplete" {
			kind = "response." + status
		}
		wrapped, err := sjson.SetRawBytes([]byte(`{}`), "response", raw)
		if err != nil {
			return err
		}
		wrapped, _ = sjson.SetBytes(wrapped, "type", kind)
		return bridge.Stream(bytes.NewReader(append(append([]byte("data: "), wrapped...), '\n', '\n')), emit)
	}
	return bridge.Stream(response.Body, emit)
}

func (e *BasispointsExecutor) publish(ctx context.Context, reporter *helps.UsageReporter, response []byte) {
	reporter.SetResponseModel(gjson.GetBytes(response, "model").String())
	if gjson.GetBytes(response, "usage").Exists() {
		reporter.Publish(ctx, helps.ParseOpenAIUsage(response))
	} else {
		reporter.EnsurePublished(ctx)
	}
}

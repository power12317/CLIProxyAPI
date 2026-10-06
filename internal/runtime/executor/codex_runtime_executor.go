package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexRuntimeExecutor delegates through the master to the process for the selected CPA credential.
type CodexRuntimeExecutor struct{ cfg *config.Config }

func NewCodexRuntimeExecutor(cfg *config.Config) *CodexRuntimeExecutor {
	return &CodexRuntimeExecutor{cfg: cfg}
}
func (e *CodexRuntimeExecutor) RequestToFormat(coreexecutor.Request, coreexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatCodex
}
func (e *CodexRuntimeExecutor) Identifier() string { return coreauth.CodexRuntimeProvider }
func runtimeError(status int, msg string) error    { return &bridge.Error{Status: status, Message: msg} }
func (e *CodexRuntimeExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, runtimeError(409, "OAuth refresh is owned by the Codex runtime")
}
func (e *CodexRuntimeExecutor) PrepareRequest(*http.Request, *coreauth.Auth) error {
	return runtimeError(400, "Codex runtime does not expose bearer credentials")
}
func (e *CodexRuntimeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, runtimeError(400, "Arbitrary HTTP requests are not supported by the Codex runtime")
}
func (e *CodexRuntimeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, runtimeError(400, "Token counting is not supported by the Codex runtime")
}

func (e *CodexRuntimeExecutor) prepare(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, upstreamLog *helps.CodexRuntimeLog) (*bridge.Client, []byte, error) {
	if e.cfg == nil || !bridge.Enabled(e.cfg) || e.cfg.Home.Enabled || !coreauth.IsCodexRuntimeOwnedAuth(auth) || auth.Disabled {
		return nil, nil, runtimeError(503, "Codex runtime is not enabled for this credential")
	}
	switch coreexecutor.ResponseFormatOrSource(opts) {
	case sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions:
	default:
		return nil, nil, runtimeError(400, "Unsupported Codex runtime response protocol")
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	if coreexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, nil, runtimeError(400, "Persistent upstream WebSocket steering is not supported by the Codex runtime")
	}
	if opts.ProxyURL != "" {
		return nil, nil, runtimeError(400, "Configure the Codex runtime network instead of per-request proxy overrides")
	}
	operation := "responses"
	if opts.Alt != "" {
		if opts.Alt != "responses/compact" {
			return nil, nil, runtimeError(400, "Codex runtime operation is unsupported")
		}
		operation = opts.Alt
	}
	if isCodexOpenAIImageRequest(opts) {
		return nil, nil, runtimeError(400, "Image endpoints require a dedicated Codex runtime capability")
	}
	if !json.Valid(req.Payload) || !gjson.ParseBytes(req.Payload).IsObject() {
		return nil, nil, runtimeError(400, "Request must be a JSON object")
	}
	body := bytes.Clone(req.Payload)
	if opts.SourceFormat != sdktranslator.FormatCodex && opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		switch opts.SourceFormat {
		case sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions:
			body = sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatCodex, model, body, true)
		default:
			return nil, nil, runtimeError(400, "Unsupported Codex runtime source protocol")
		}
	}
	var err error
	body, err = helps.ApplyRequestThinking(body, req, opts, opts.SourceFormat.String(), sdktranslator.FormatCodex.String(), "codex")
	if err != nil {
		return nil, nil, runtimeError(400, "Invalid Codex runtime reasoning configuration")
	}
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, nil, runtimeError(400, "Invalid Codex runtime request")
	}
	if operation == "responses" {
		body, _ = sjson.SetBytes(body, "stream", true)
	}
	scope := helps.APIKeyFromContext(ctx)
	if scope == "" {
		scope, _ = opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	}
	session, _ := opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey].(string)
	if session == "" {
		session, _ = opts.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string)
	}
	if session == "" {
		session = gjson.GetBytes(body, "prompt_cache_key").String()
	}
	if session == "" || scope == "" {
		session = uuid.NewString()
	}
	identityJSON, _ := json.Marshal([]string{scope, auth.ID, session})
	identity := sha256.Sum256(identityJSON)
	session = hex.EncodeToString(identity[:])
	body, _ = sjson.SetBytes(body, "prompt_cache_key", session)
	client, err := bridge.Dial(ctx, e.cfg.Codex.Runtime.Endpoint())
	if err != nil {
		return nil, nil, err
	}
	client.OnUpstream = upstreamLog.Record
	upstreamLog.RawBody = true
	requestID, err := uuid.NewV7()
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	err = client.Start(bridge.Request{RequestID: requestID.String(), CredentialID: auth.ID, Operation: operation, SourceFormat: opts.SourceFormat.String(), SessionID: session, Request: body})
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	coreexecutor.ReportUpstreamWebsocket(ctx, false)
	return client, body, nil
}

func (e *CodexRuntimeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (resp coreexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, thinking.ParseSuffix(req.Model).ModelName)
	defer upstreamLog.Flush()
	defer func() {
		if err != nil {
			upstreamLog.Error(err)
		}
	}()
	client, body, err := e.prepare(ctx, auth, req, opts, upstreamLog)
	if err != nil {
		return resp, err
	}
	defer client.Close()
	client.Headers.Set("Content-Type", "application/json")
	reader := bridge.NewSSEReader(&bridge.BodyReader{Client: client})
	outputItems := make(map[int64][]byte)
	var outputFallback [][]byte
	var terminal []byte
	sawOutputDelta := false
	for {
		frame, errNext := reader.Next()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return resp, errNext
		}
		event := frame.Data
		if len(event) == 0 || bytes.Equal(event, []byte("[DONE]")) {
			continue
		}
		upstreamLog.Event(event)
		reporter.ObserveCodexResponseModel(event)
		if failure, failureBody, ok := codexTerminalFailureErr(event); ok {
			return resp, &bridge.Error{Status: failure.StatusCode(), Message: failure.Error(), Body: failureBody, ResponseHeaders: client.Headers.Clone()}
		}
		sawOutputDelta = sawOutputDelta || helps.HasMeaningfulCodexOutputDelta(event)
		if helps.IsCodexTerminalEmptyIncomplete(event, len(outputItems)+len(outputFallback), sawOutputDelta) {
			failure := newCodexEmptyIncompleteStreamError()
			return resp, runtimeError(failure.StatusCode(), failure.Error())
		}
		switch gjson.GetBytes(event, "type").String() {
		case "response.output_item.done":
			collectCodexOutputItemDone(event, outputItems, &outputFallback)
		case "response.completed", "response.incomplete", "response.done":
			terminal = patchCodexCompletedOutput(normalizeCodexWebsocketCompletion(event), outputItems, outputFallback)
		}
	}
	if len(terminal) == 0 {
		failure := newCodexIncompleteStreamError()
		return resp, runtimeError(failure.StatusCode(), failure.Error())
	}
	if detail, ok := helps.ParseCodexUsage(terminal); ok {
		reporter.Publish(ctx, detail)
	}
	publishCodexImageToolUsage(ctx, reporter, body, terminal)
	format := coreexecutor.ResponseFormatOrSource(opts)
	var output []byte
	if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex {
		output = []byte(gjson.GetBytes(terminal, "response").Raw)
		if format == sdktranslator.FormatOpenAIResponse {
			output = helps.EnsureResponsesUsageDetails(output)
		}
	} else {
		var param any
		original := req.Payload
		if len(opts.OriginalRequest) > 0 {
			original = opts.OriginalRequest
		}
		output = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, format, req.Model, original, body, terminal, &param)
	}
	reporter.EnsurePublished(ctx)
	return coreexecutor.Response{Payload: output, Headers: client.Headers}, nil
}

func (e *CodexRuntimeExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if opts.Alt == "responses/compact" {
		return nil, runtimeError(400, "Compaction is not a streaming operation")
	}
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	reporter.SetStream(true)
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, thinking.ParseSuffix(req.Model).ModelName)
	client, body, err := e.prepare(ctx, auth, req, opts, upstreamLog)
	if err != nil {
		upstreamLog.Error(err)
		reporter.PublishFailure(ctx, err)
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer client.Close()
		defer upstreamLog.Flush()
		send := func(chunk coreexecutor.StreamChunk) bool {
			select {
			case chunks <- chunk:
				return true
			case <-ctx.Done():
				if chunk.Err == nil {
					upstreamLog.Error(ctx.Err())
				}
				return false
			}
		}
		var param any
		original := req.Payload
		if len(opts.OriginalRequest) > 0 {
			original = opts.OriginalRequest
		}
		format := coreexecutor.ResponseFormatOrSource(opts)
		reader := bridge.NewSSEReader(&bridge.BodyReader{Client: client})
		claudeTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, sdktranslator.FormatCodex, format, original)
		outputItems := make(map[int64][]byte)
		var outputFallback [][]byte
		sawOutputDelta, terminal := false, false
		fail := func(err error) {
			upstreamLog.Error(err)
			reporter.PublishFailure(ctx, err)
			send(coreexecutor.StreamChunk{Err: err})
		}
		for {
			frame, errNext := reader.Next()
			if errNext == io.EOF {
				if !terminal {
					failure := newCodexIncompleteStreamError()
					fail(runtimeError(failure.StatusCode(), failure.Error()))
				} else {
					reporter.EnsurePublished(ctx)
				}
				return
			}
			if errNext != nil {
				fail(errNext)
				return
			}
			event := frame.Data
			if len(event) == 0 || bytes.Equal(event, []byte("[DONE]")) {
				if (format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex) && len(frame.Wire) > 0 {
					if !send(coreexecutor.StreamChunk{Payload: frame.Wire}) {
						return
					}
				}
				continue
			}
			upstreamLog.Event(event)
			observeCodexTokenEvent(reporter, event)
			if failure, failureBody, ok := codexTerminalFailureErr(event); ok {
				fail(&bridge.Error{Status: failure.StatusCode(), Message: failure.Error(), Body: failureBody, ResponseHeaders: client.Headers.Clone()})
				return
			}
			sawOutputDelta = sawOutputDelta || helps.HasMeaningfulCodexOutputDelta(event)
			if helps.IsCodexTerminalEmptyIncomplete(event, len(outputItems)+len(outputFallback), sawOutputDelta) {
				failure := newCodexEmptyIncompleteStreamError()
				fail(runtimeError(failure.StatusCode(), failure.Error()))
				return
			}
			switch gjson.GetBytes(event, "type").String() {
			case "response.output_item.done":
				collectCodexOutputItemDone(event, outputItems, &outputFallback)
			case "response.completed", "response.incomplete", "response.done":
				terminal = true
				event = normalizeCodexWebsocketCompletion(event)
				if detail, ok := helps.ParseCodexUsage(event); ok {
					reporter.Publish(ctx, detail)
				}
				publishCodexImageToolUsage(ctx, reporter, body, event)
				if !helps.IsNativeCodexRequest(req.Payload, opts) {
					event = patchCodexCompletedOutput(event, outputItems, outputFallback)
				}
			}
			if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex {
				if !bytes.Equal(event, frame.Data) {
					frame.Wire = append(append([]byte("data: "), event...), '\n', '\n')
				}
				if !send(coreexecutor.StreamChunk{Payload: frame.Wire}) {
					return
				}
			} else {
				// Translators accept one data line; fold multiline SSE JSON first.
				var compact bytes.Buffer
				if json.Compact(&compact, event) == nil {
					event = compact.Bytes()
				}
				line := append([]byte("data: "), event...)
				for _, chunk := range helps.TranslateStreamWithClaudeInputTokens(ctx, sdktranslator.FormatCodex, format, req.Model, original, body, line, &param, claudeTokens) {
					if !send(coreexecutor.StreamChunk{Payload: chunk}) {
						return
					}
				}
			}
		}
	}()
	return &coreexecutor.StreamResult{Headers: client.Headers, Chunks: chunks}, nil
}

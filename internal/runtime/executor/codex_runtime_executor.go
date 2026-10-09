package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
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

func (e *CodexRuntimeExecutor) validate(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) error {
	if e.cfg == nil || !bridge.Enabled(e.cfg) || e.cfg.Home.Enabled || !coreauth.IsCodexRuntimeOwnedAuth(auth) || auth.Disabled {
		return runtimeError(503, "Codex runtime is not enabled for this credential")
	}
	switch coreexecutor.ResponseFormatOrSource(opts) {
	case sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions, codexOpenAIImageSourceFormat:
	default:
		return runtimeError(400, "Unsupported Codex runtime response protocol")
	}
	if coreexecutor.RequiredUpstreamWebsocket(ctx) {
		return runtimeError(400, "Persistent upstream WebSocket steering is not supported by the Codex runtime")
	}
	if opts.ProxyURL != "" {
		return runtimeError(400, "Configure the Codex runtime network instead of per-request proxy overrides")
	}
	if opts.Alt != "" {
		if opts.Alt != "responses/compact" {
			return runtimeError(400, "Codex runtime operation is unsupported")
		}
	}
	if isCodexOpenAIImageRequest(opts) {
		// The image builder validates and translates JSON or multipart before IPC.
		return nil
	}
	if !json.Valid(req.Payload) || !gjson.ParseBytes(req.Payload).IsObject() {
		return runtimeError(400, "Request must be a JSON object")
	}
	return nil
}

func (e *CodexRuntimeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	if err := e.validate(ctx, auth, req, opts); err != nil {
		return coreexecutor.Response{}, err
	}
	return (&CodexExecutor{cfg: e.cfg, runtime: e}).Execute(ctx, auth, req, opts)
}

func (e *CodexRuntimeExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if opts.Alt == "responses/compact" {
		return nil, runtimeError(400, "Compaction is not a streaming operation")
	}
	if err := e.validate(ctx, auth, req, opts); err != nil {
		return nil, err
	}
	return (&CodexExecutor{cfg: e.cfg, runtime: e}).ExecuteStream(ctx, auth, req, opts)
}

// start passes the complete CPA request to the worker for native request construction.
func (e *CodexRuntimeExecutor) start(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, body []byte, upstreamLog *helps.CodexRuntimeLog) (*bridge.Client, error) {
	return e.startOperation(ctx, auth, req, opts, body, upstreamLog, "")
}

func (e *CodexRuntimeExecutor) startOperation(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, body []byte, upstreamLog *helps.CodexRuntimeLog, imageAPI string) (*bridge.Client, error) {
	operation := "responses"
	if opts.Alt != "" {
		operation = opts.Alt
	}
	if imageAPI != "" {
		operation = "images/generations"
		if strings.HasSuffix(helps.PayloadRequestPath(opts), codexImagesEditsPath) {
			operation = "images/edits"
		}
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
		session = gjson.GetBytes(req.Payload, "prompt_cache_key").String()
	}
	if session == "" || scope == "" {
		session = uuid.NewString()
	}
	identityJSON, _ := json.Marshal([]string{scope, auth.ID, session})
	identity := sha256.Sum256(identityJSON)
	session = hex.EncodeToString(identity[:])
	client, err := bridge.Dial(ctx, e.cfg.Codex.Runtime.Endpoint())
	if err != nil {
		return nil, err
	}
	client.OnUpstream = upstreamLog.Record
	upstreamLog.RawBody = true
	requestID := ""
	if imageAPI != "" {
		requestID = logging.GetRequestID(ctx)
	}
	if requestID == "" {
		id, errID := uuid.NewV7()
		if errID != nil {
			client.Close()
			return nil, errID
		}
		requestID = id.String()
	}
	err = client.Start(bridge.Request{RequestID: requestID, CredentialID: auth.ID, Operation: operation, SourceFormat: opts.SourceFormat.String(), SessionID: session, Request: body, ImageAPI: imageAPI})
	if err != nil {
		client.Close()
		return nil, err
	}
	coreexecutor.ReportUpstreamWebsocket(ctx, false)
	return client, nil
}

func (e *CodexRuntimeExecutor) executeViaMaster(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter, body []byte, optimizeMultiAgentV2 bool, replayScope codexReasoningReplayScope) (resp coreexecutor.Response, err error) {
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, thinking.ParseSuffix(req.Model).ModelName)
	defer upstreamLog.Flush()
	defer func() {
		if err != nil {
			var remote *bridge.Error
			if errors.As(err, &remote) {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, remote.StatusCode(), remote.Body); errClearReplay != nil {
					err = errClearReplay
				}
			}
			upstreamLog.Error(err)
		}
	}()
	client, err := e.start(ctx, auth, req, opts, body, upstreamLog)
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
		event := helps.RestoreCodexMultiAgentV2Response(frame.Data, optimizeMultiAgentV2)
		if len(event) == 0 || bytes.Equal(event, []byte("[DONE]")) {
			continue
		}
		upstreamLog.Event(event)
		reporter.ObserveResponseModel(event)
		if failure, failureBody, ok := codexTerminalFailureErrWithCooling(event, e.cfg.Codex.ModelLevelCooling); ok {
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
			if gjson.GetBytes(terminal, "type").String() == "response.completed" {
				cacheCodexReasoningReplayFromCompleted(replayScope, terminal)
			}
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

func (e *CodexRuntimeExecutor) executeStreamViaMaster(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter, body []byte, optimizeMultiAgentV2 bool, replayScope codexReasoningReplayScope) (*coreexecutor.StreamResult, error) {
	reporter.SetStream(true)
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, thinking.ParseSuffix(req.Model).ModelName)
	client, err := e.start(ctx, auth, req, opts, body, upstreamLog)
	if err != nil {
		var remote *bridge.Error
		if errors.As(err, &remote) {
			if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, remote.StatusCode(), remote.Body); errClearReplay != nil {
				err = errClearReplay
			}
		}
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
			var remote *bridge.Error
			if errors.As(err, &remote) {
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, remote.StatusCode(), remote.Body); errClearReplay != nil {
					err = errClearReplay
				}
			}
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
			event := helps.RestoreCodexMultiAgentV2Response(frame.Data, optimizeMultiAgentV2)
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
			if failure, failureBody, ok := codexTerminalFailureErrWithCooling(event, e.cfg.Codex.ModelLevelCooling); ok {
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
				if gjson.GetBytes(event, "type").String() == "response.completed" {
					cacheCodexReasoningReplayFromCompleted(replayScope, event)
				}
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

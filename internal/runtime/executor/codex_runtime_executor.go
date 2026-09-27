package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexRuntimeExecutor delegates to a separately deployed managed-auth worker.
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

func (e *CodexRuntimeExecutor) prepare(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*bridge.Client, []byte, error) {
	if e.cfg == nil || !e.cfg.Codex.Runtime.Enabled || e.cfg.Home.Enabled || !coreauth.IsCodexRuntimeAuth(auth) || auth.Disabled {
		return nil, nil, runtimeError(503, "Codex runtime is not enabled for this credential")
	}
	var worker *config.CodexRuntimeWorker
	for i := range e.cfg.Codex.Runtime.Workers {
		w := &e.cfg.Codex.Runtime.Workers[i]
		if w.ID == auth.Attributes[coreauth.AttributeCodexRuntimeID] {
			worker = w
			break
		}
	}
	if worker == nil || worker.Disabled || auth.ID != "codex-runtime:"+worker.ID {
		return nil, nil, runtimeError(503, "Codex runtime worker is unavailable")
	}
	if auth.Attributes["account_id"] != worker.AccountID || auth.Attributes["socket"] != worker.Socket {
		return nil, nil, runtimeError(409, "Codex runtime reference changed; select the updated credential")
	}
	switch coreexecutor.ResponseFormatOrSource(opts) {
	case sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions:
	default:
		return nil, nil, runtimeError(400, "Unsupported Codex runtime response protocol")
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	allowed := false
	for _, m := range worker.Models {
		if m == model {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, runtimeError(400, "Model is not configured for the Codex runtime worker")
	}
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
	identityJSON, _ := json.Marshal([]string{scope, worker.ID, worker.AccountID, session})
	identity := sha256.Sum256(identityJSON)
	session = hex.EncodeToString(identity[:])
	body, _ = sjson.SetBytes(body, "prompt_cache_key", session)
	client, err := bridge.Open(ctx, worker.Socket, worker.ID, worker.AccountID)
	if err != nil {
		return nil, nil, err
	}
	err = client.Start(bridge.Request{RequestID: uuid.NewString(), CredentialID: worker.ID, AccountID: worker.AccountID, Operation: operation, SourceFormat: opts.SourceFormat.String(), SessionID: session, Request: body})
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	coreexecutor.ReportUpstreamWebsocket(ctx, false)
	return client, body, nil
}

func (e *CodexRuntimeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (resp coreexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	client, body, err := e.prepare(ctx, auth, req, opts)
	if err != nil {
		return resp, err
	}
	defer client.Close()
	client.Headers.Set("Content-Type", "application/json")
	var terminal, compact []byte
	for {
		frame, errNext := client.Next()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return resp, errNext
		}
		if len(frame.Body) > 0 {
			compact = frame.Body
			continue
		}
		reporter.ObserveCodexResponseModel(frame.Event)
		switch gjson.GetBytes(frame.Event, "type").String() {
		case "error":
			return resp, runtimeError(502, "Codex runtime returned an upstream error event")
		case "response.completed", "response.incomplete", "response.failed":
			terminal = frame.Event
		}
	}
	if len(compact) > 0 {
		reporter.EnsurePublished(ctx)
		return coreexecutor.Response{Payload: compact, Headers: client.Headers}, nil
	}
	if len(terminal) == 0 {
		return resp, runtimeError(502, "Codex runtime response did not complete")
	}
	format := coreexecutor.ResponseFormatOrSource(opts)
	if gjson.GetBytes(terminal, "type").String() == "response.failed" {
		errFailed := runtimeError(502, "Codex runtime model response failed")
		detail, _ := helps.ParseCodexUsage(terminal)
		reporter.PublishFailureWithDetail(ctx, detail, errFailed)
		if format != sdktranslator.FormatCodex && format != sdktranslator.FormatOpenAIResponse {
			return resp, errFailed
		}
	} else if detail, ok := helps.ParseCodexUsage(terminal); ok {
		reporter.Publish(ctx, detail)
	}
	var output []byte
	if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex {
		output = []byte(gjson.GetBytes(terminal, "response").Raw)
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
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	reporter.SetStream(true)
	client, body, err := e.prepare(ctx, auth, req, opts)
	if err != nil {
		reporter.PublishFailure(ctx, err)
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer client.Close()
		send := func(chunk coreexecutor.StreamChunk) bool {
			select {
			case chunks <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var param any
		original := req.Payload
		if len(opts.OriginalRequest) > 0 {
			original = opts.OriginalRequest
		}
		format := coreexecutor.ResponseFormatOrSource(opts)
		for {
			frame, errNext := client.Next()
			if errNext == io.EOF {
				reporter.EnsurePublished(ctx)
				return
			}
			if errNext != nil {
				reporter.PublishFailure(ctx, errNext)
				send(coreexecutor.StreamChunk{Err: errNext})
				return
			}
			if len(frame.Event) == 0 {
				reporter.PublishFailure(ctx, fmt.Errorf("unexpected runtime result"))
				send(coreexecutor.StreamChunk{Err: runtimeError(502, "Unexpected Codex runtime result")})
				return
			}
			reporter.ObserveCodexResponseModel(frame.Event)
			eventType := gjson.GetBytes(frame.Event, "type").String()
			if eventType == "response.failed" || eventType == "error" {
				errFailed := runtimeError(502, "Codex runtime model response failed")
				detail, _ := helps.ParseCodexUsage(frame.Event)
				reporter.PublishFailureWithDetail(ctx, detail, errFailed)
				if format != sdktranslator.FormatCodex && format != sdktranslator.FormatOpenAIResponse {
					send(coreexecutor.StreamChunk{Err: errFailed})
					return
				}
			} else if detail, ok := helps.ParseCodexUsage(frame.Event); ok {
				reporter.Publish(ctx, detail)
			}
			line := append([]byte("data: "), frame.Event...)
			if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex {
				line = append(line, '\n', '\n')
				if !send(coreexecutor.StreamChunk{Payload: line}) {
					return
				}
			} else {
				for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, format, req.Model, original, body, line, &param) {
					if !send(coreexecutor.StreamChunk{Payload: chunk}) {
						return
					}
				}
			}
		}
	}()
	return &coreexecutor.StreamResult{Headers: client.Headers, Chunks: chunks}, nil
}

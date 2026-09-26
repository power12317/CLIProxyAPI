// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

// CodexAutoExecutor selects upstream transport without changing downstream framing.
type CodexAutoExecutor struct {
	httpExec        *CodexExecutor
	wsExec          *CodexWebsocketsExecutor
	basispointsExec *BasispointsExecutor
	poolOnce        sync.Once
	pool            *cliproxyexecutor.ExecutionSessionPool
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec:        NewCodexExecutor(cfg),
		wsExec:          NewCodexWebsocketsExecutor(cfg),
		basispointsExec: NewBasispointsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e != nil && e.basispointsExec.enabled() {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if reason := basispoints.NativeToolReason(req.Payload); reason != "" {
			helps.LogBasispointsNativeToolRoute(ctx, reason)
		} else {
			return e.basispointsExec.Execute(ctx, auth, req, opts)
		}
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if e.forceWebsocket(auth) && opts.Alt != "responses/compact" {
		ctx, opts, release := e.prepareForcedWebsocket(ctx, req, opts)
		defer release()
		state := cliproxyexecutor.CodexTransport(ctx)
		if !state.HTTPFallback.Load() {
			cliproxyexecutor.ReportUpstreamWebsocket(ctx, true)
			response, err := e.wsExec.Execute(ctx, auth, req, opts)
			if !errors.Is(err, cliproxyexecutor.ErrCodexWebsocketFallback) {
				return response, err
			}
		}
		if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) || gjson.GetBytes(req.Payload, "previous_response_id").String() != "" {
			return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		return e.httpExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e != nil && e.basispointsExec.enabled() {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if reason := basispoints.NativeToolReason(req.Payload); reason != "" {
			helps.LogBasispointsNativeToolRoute(ctx, reason)
		} else {
			return e.basispointsExec.ExecuteStream(ctx, auth, req, opts)
		}
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if e.forceWebsocket(auth) && opts.Alt != "responses/compact" {
		ctx, opts, release := e.prepareForcedWebsocket(ctx, req, opts)
		state := cliproxyexecutor.CodexTransport(ctx)
		var result *cliproxyexecutor.StreamResult
		var err error
		if !state.HTTPFallback.Load() {
			cliproxyexecutor.ReportUpstreamWebsocket(ctx, true)
			result, err = e.wsExec.ExecuteStream(ctx, auth, req, opts)
		}
		if state.HTTPFallback.Load() && (err == nil || errors.Is(err, cliproxyexecutor.ErrCodexWebsocketFallback)) {
			if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) || gjson.GetBytes(req.Payload, "previous_response_id").String() != "" {
				release()
				return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
			}
			cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
			result = helps.CodexWebsocketPrewarmFallback(ctx, req)
			if result == nil {
				result, err = e.httpExec.ExecuteStream(ctx, auth, req, opts)
			} else {
				err = nil
			}
		}
		if err != nil {
			release()
			return nil, err
		}
		return cliproxyexecutor.ReleaseSessionAfterStream(ctx, result, release), nil
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.poolOnce.Do(func() { e.pool = cliproxyexecutor.NewExecutionSessionPool(e.CloseExecutionSession) })
		e.pool.Drain()
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	// Forced mode owns recovery in the ordered request stream. An asynchronous
	// upstream close must not close a healthy downstream connection first.
	if e.httpExec.cfg != nil && (e.httpExec.cfg.Codex.ForceWebsocket || e.basispointsExec.enabled()) {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func (e *CodexAutoExecutor) forceWebsocket(auth *cliproxyauth.Auth) bool {
	return e.httpExec.cfg != nil && e.httpExec.cfg.Codex.ForceWebsocket && auth != nil &&
		cliproxyexecutor.ChatGPTCodexDestination(auth.Provider, auth.Attributes["base_url"])
}

func (e *CodexAutoExecutor) prepareForcedWebsocket(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Options, func()) {
	state := cliproxyexecutor.EnsureCodexTransport(&opts)
	ctx = cliproxyexecutor.WithCodexTransport(ctx, state)
	if executionSessionIDFromOptions(opts) != "" {
		return ctx, opts, func() {}
	}
	e.poolOnce.Do(func() { e.pool = cliproxyexecutor.NewExecutionSessionPool(e.CloseExecutionSession) })
	id, release := e.pool.Acquire(cliproxyexecutor.CodexSessionPoolKey(req, opts))
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = id
	opts.Metadata = metadata
	return ctx, opts, release
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

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

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
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

// Large histories are sent as one upstream WebSocket message. Keep requests at
// or above Basispoints' 16 MiB limit on the POST/SSE transport to avoid
// upstream 1009 closes.
const codexWebsocketRequestSizeLimit = 16 << 20

func codexWebsocketRequestTooLarge(payload []byte) bool {
	return len(payload) >= codexWebsocketRequestSizeLimit
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec:        NewCodexExecutor(cfg),
		wsExec:          NewCodexWebsocketsExecutor(cfg),
		basispointsExec: NewBasispointsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

// UsesConfig reports whether the executor was created for cfg.
func (e *CodexAutoExecutor) UsesConfig(cfg *config.Config) bool {
	return e != nil && e.httpExec != nil && e.httpExec.cfg == cfg
}

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	fresh, _, err := bridge.LoadSharedAuth(e.httpExec.cfg, auth)
	if err != nil {
		return err
	}
	if cliproxyauth.IsCodexRuntimeOwnedAuth(fresh) {
		return runtimeError(400, "Use the Codex runtime for this credential")
	}
	auth = fresh
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	ctx, auth, release, remote, err := bridge.PrepareShared(ctx, e.httpExec.cfg, auth)
	if err != nil {
		return nil, err
	}
	if remote {
		release()
		return nil, runtimeError(400, "Arbitrary HTTP requests are unavailable in Codex runtime mode")
	}
	resp, err := e.httpExec.HttpRequest(ctx, auth, req.WithContext(ctx))
	if err != nil || resp == nil || resp.Body == nil {
		release()
	} else {
		resp.Body = bridge.ReleaseBody(resp.Body, release)
	}
	return resp, err
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if isCodexOpenAIImageRequest(opts) {
		ctx = cliproxyexecutor.WithCodexRuntimeRoute(ctx)
	}
	if isCodexOpenAIImageRequest(opts) && e != nil && e.httpExec != nil && bridge.Enabled(e.httpExec.cfg) && cliproxyauth.IsCodexRuntimeOwnedAuth(auth) {
		cliproxyexecutor.RequireCodexRuntimeRoute(ctx)
	}
	if e != nil && e.httpExec != nil {
		var release func()
		var remote bool
		var err error
		ctx, auth, release, remote, err = bridge.PrepareShared(ctx, e.httpExec.cfg, auth)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		defer release()
		if remote {
			if isCodexOpenAIImageRequest(opts) {
				cliproxyexecutor.RequireCodexRuntimeRoute(ctx)
			}
			return NewCodexRuntimeExecutor(e.httpExec.cfg).Execute(ctx, auth, req, opts)
		}
	}
	if isCodexOpenAIImageRequest(opts) && cliproxyexecutor.CodexRuntimeRouteRequired(ctx) {
		return cliproxyexecutor.Response{}, runtimeError(503, "Image request requires a Codex runtime credential")
	}
	if e != nil && e.httpExec != nil && isCodexOpenAIImageRequest(opts) {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if e.basispointsExec.enabled() {
			helps.LogBasispointsNativeToolRoute(ctx, "openai_image_request")
		}
		return e.httpExec.Execute(ctx, auth, req, opts)
	}
	if e != nil && e.basispointsExec.enabled() {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if reason := basispoints.NativeToolReason(req.Payload); reason != "" {
			helps.LogBasispointsNativeToolRoute(ctx, reason)
		} else {
			result, err := e.basispointsExec.Execute(ctx, auth, req, opts)
			if reason := basispoints.RequestedNativeTool(err); reason != "" {
				if ctx.Err() != nil {
					return cliproxyexecutor.Response{}, ctx.Err()
				}
				helps.LogBasispointsNativeToolRoute(ctx, reason)
			} else {
				return result, err
			}
		}
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if codexWebsocketRequestTooLarge(req.Payload) {
		helps.LogWithRequestID(ctx).Infof("codex websockets: upstream request is too large for websocket bytes=%d limit=%d; using POST", len(req.Payload), codexWebsocketRequestSizeLimit)
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		return e.httpExec.Execute(ctx, auth, req, opts)
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
	if isCodexOpenAIImageRequest(opts) {
		ctx = cliproxyexecutor.WithCodexRuntimeRoute(ctx)
	}
	if isCodexOpenAIImageRequest(opts) && e != nil && e.httpExec != nil && bridge.Enabled(e.httpExec.cfg) && cliproxyauth.IsCodexRuntimeOwnedAuth(auth) {
		cliproxyexecutor.RequireCodexRuntimeRoute(ctx)
	}
	if e != nil && e.httpExec != nil {
		sharedCtx, fresh, release, remote, err := bridge.PrepareShared(ctx, e.httpExec.cfg, auth)
		if err != nil {
			return nil, err
		}
		if remote {
			if isCodexOpenAIImageRequest(opts) {
				cliproxyexecutor.RequireCodexRuntimeRoute(sharedCtx)
			}
			result, err := NewCodexRuntimeExecutor(e.httpExec.cfg).ExecuteStream(sharedCtx, fresh, req, opts)
			if err != nil {
				release()
				return nil, err
			}
			return cliproxyexecutor.ReleaseSessionAfterStream(sharedCtx, result, release), nil
		}
		result, err := e.executeNativeStream(sharedCtx, fresh, req, opts)
		if err != nil {
			release()
			return nil, err
		}
		return cliproxyexecutor.ReleaseSessionAfterStream(sharedCtx, result, release), nil
	}
	return nil, fmt.Errorf("codex auto executor: executor is nil")
}

func (e *CodexAutoExecutor) executeNativeStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if isCodexOpenAIImageRequest(opts) && cliproxyexecutor.CodexRuntimeRouteRequired(ctx) {
		return nil, runtimeError(503, "Image request requires a Codex runtime credential")
	}
	if e != nil && e.httpExec != nil && isCodexOpenAIImageRequest(opts) {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if e.basispointsExec.enabled() {
			helps.LogBasispointsNativeToolRoute(ctx, "openai_image_request")
		}
		return e.httpExec.ExecuteStream(ctx, auth, req, opts)
	}
	if e != nil && e.basispointsExec.enabled() {
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		if reason := basispoints.NativeToolReason(req.Payload); reason != "" {
			helps.LogBasispointsNativeToolRoute(ctx, reason)
		} else {
			result, err := e.basispointsExec.ExecuteStream(ctx, auth, req, opts)
			if err == nil && basispoints.OptionalNativeTools(req.Payload) {
				result, err = helps.BufferNativeToolDecision(ctx, result)
			}
			if reason := basispoints.RequestedNativeTool(err); reason != "" {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				helps.LogBasispointsNativeToolRoute(ctx, reason)
			} else {
				return result, err
			}
		}
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if codexWebsocketRequestTooLarge(req.Payload) {
		helps.LogWithRequestID(ctx).Infof("codex websockets: upstream request is too large for websocket bytes=%d limit=%d; using POST/SSE", len(req.Payload), codexWebsocketRequestSizeLimit)
		cliproxyexecutor.ReportUpstreamWebsocket(ctx, false)
		return e.httpExec.ExecuteStream(ctx, auth, req, opts)
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
	fresh, _, err := bridge.LoadSharedAuth(e.httpExec.cfg, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if cliproxyauth.IsCodexRuntimeOwnedAuth(fresh) {
		return cliproxyexecutor.Response{}, runtimeError(400, "Token counting is unavailable in Codex runtime mode")
	}
	auth = fresh
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		if e.basispointsExec != nil {
			e.basispointsExec.websocketSessions.CloseAll()
		}
		e.poolOnce.Do(func() { e.pool = cliproxyexecutor.NewExecutionSessionPool(e.CloseExecutionSession) })
		e.pool.Drain()
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) SharesExecutionSessionsWith(other cliproxyauth.ProviderExecutor) bool {
	if e == nil || e.wsExec == nil {
		return false
	}
	return e.wsExec.SharesExecutionSessionsWith(other)
}

func (e *CodexAutoExecutor) CloseAuthExecutionSessions(authID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseAuthExecutionSessions(authID)
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

// SupportsApplyPatch requires both selectable transports to support the tool.
func (e *CodexAutoExecutor) SupportsApplyPatch() bool {
	return e != nil && e.httpExec != nil && e.wsExec != nil && e.httpExec.SupportsApplyPatch() && e.wsExec.SupportsApplyPatch()
}

// InterruptExecutionSession forwards a response.interrupt control frame on the
// current upstream socket. It does not dial, replay, or select another credential.
func (e *CodexAutoExecutor) InterruptExecutionSession(ctx context.Context, sessionID string, payload []byte) error {
	if e == nil || e.wsExec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	return e.wsExec.InterruptExecutionSession(ctx, sessionID, payload)
}

// InterruptExecutionSession writes the original interrupt payload to the session
// socket captured for this execution. Payload rules and response.create defaults
// must not rewrite response_id, mode, or extension fields.
func (e *CodexWebsocketsExecutor) InterruptExecutionSession(ctx context.Context, sessionID string, payload []byte) error {
	if e == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	sess := store.sessions[sessionID]
	store.mu.Unlock()
	if sess == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	sess.connMu.Unlock()
	if conn == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	// Account checks may call back into the auth manager; do not hold session
	// locks across them. Take the lifecycle snapshot only after they finish.
	authEnabled := cliproxyexecutor.WebsocketAuthEnabled(ctx, authID)
	sess.connMu.Lock()
	if sess.conn != conn {
		sess.connMu.Unlock()
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sess.activeMu.Lock()
	_, alreadyTerminal := sess.terminalResponseIDs[gjson.GetBytes(payload, "response_id").String()]
	alreadyTerminal = alreadyTerminal && sess.responseStateConn == conn
	active := sess.activeConn == conn && sess.activeCh != nil
	var disconnectErr error
	if sess.terminalConn == conn {
		disconnectErr = sess.terminalErr
	}
	// Terminal state and the active channel must belong to one snapshot:
	// completion can otherwise land between the checks and produce a stale 400.
	sess.activeMu.Unlock()
	sess.connMu.Unlock()
	if disconnectErr != nil {
		return disconnectErr
	}
	if alreadyTerminal {
		log.WithField("outcome", "already_terminal").Debug("codex websockets: response.interrupt handled")
		return nil
	}
	// A retained socket from an earlier turn is not the current upstream.
	// HTTP turns must fall through to local cancellation instead.
	if !active {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if !authEnabled {
		return fmt.Errorf("websocket credential is no longer enabled")
	}
	if errWrite := writeCodexWebsocketMessage(sess, conn, payload); errWrite != nil {
		return errWrite
	}
	log.WithField("outcome", "forwarded").Debug("codex websockets: response.interrupt handled")
	return nil
}

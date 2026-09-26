package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// A waiting request must read the latest same-turn metadata immediately before
// sending. Explicit ticket/header overrides keep their existing precedence.
func (e *CodexWebsocketsExecutor) refreshTopicTurnState(auth *cliproxyauth.Auth, state *helps.CodexTurnState, body []byte, headers http.Header, model string) ([]byte, error) {
	state.ApplyWebsocketHeaders(headers)
	body = state.ApplyWebsocketBody(body)
	if err := helps.ApplyCodexTurnStateTicket(auth, e.cfg.Codex.EffectiveTurnStateTicket(), model, headers); err != nil {
		return nil, err
	}
	body = helps.ApplyCodexTurnStateTicketBody(auth, e.cfg.Codex.EffectiveTurnStateTicket(), model, body)
	state.ObserveRequest(headers, body)
	return body, nil
}

// Topic ownership is only a connection lookup key. Keep execution metadata
// unchanged so connection reuse cannot alter the existing wire identity pipeline.
func (e *CodexWebsocketsExecutor) websocketSessionID(auth *cliproxyauth.Auth, req core.Request, opts core.Options, upstreamURL string) string {
	if auth != nil && (e.cfg != nil && e.cfg.Codex.ForceWebsocket || core.ChatGPTCodexDestination(auth.Provider, auth.Attributes["base_url"])) {
		if id := helps.CodexWebsocketTopicSessionID(auth, req, opts, upstreamURL); id != "" {
			return id
		}
	}
	return executionSessionIDFromOptions(opts)
}

func (s *codexWebsocketSession) isTopicSession() bool {
	return s != nil && strings.HasPrefix(s.sessionID, helps.CodexTopicSessionPrefix)
}

func (s *codexWebsocketSession) acquireRequest(ctx context.Context) error {
	if s.isTopicSession() {
		return s.topicActivity.Acquire(ctx)
	}
	s.reqMu.Lock()
	return nil
}

func (s *codexWebsocketSession) releaseRequest() {
	if s.isTopicSession() {
		s.topicActivity.Release()
		return
	}
	s.reqMu.Unlock()
}

// Executor replacement leaves topic-owned connections in the shared store. A
// downstream execution session or its pool lease cannot close those connections.
func (e *CodexAutoExecutor) RetireExecutionSessions() {
	if e != nil && e.wsExec != nil {
		e.poolOnce.Do(func() { e.pool = core.NewExecutionSessionPool(e.CloseExecutionSession) })
		e.pool.Drain()
		e.wsExec.RetireExecutionSessions()
	}
}

func (e *CodexWebsocketsExecutor) RetireExecutionSessions() {
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	var retired []*codexWebsocketSession
	for id, sess := range store.sessions {
		if !sess.isTopicSession() {
			delete(store.sessions, id)
			retired = append(retired, sess)
		}
	}
	store.mu.Unlock()
	for _, sess := range retired {
		closeCodexWebsocketSession(sess, "executor_replaced")
	}
}

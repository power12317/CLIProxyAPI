package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

var globalCodexCredentialWebsockets helps.CodexWebsocketRegistry

func (e *CodexWebsocketsExecutor) usesCredentialSockets(auth *cliproxyauth.Auth) bool {
	return e != nil && auth != nil && (e.credentialSockets != nil ||
		cliproxyexecutor.ChatGPTCodexDestination(auth.Provider, auth.Attributes["base_url"]))
}

func (e *CodexWebsocketsExecutor) ensureCredentialSocket(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	if sess == nil {
		return nil, nil, nil, &helps.CodexWebsocketMuxError{Message: "credential websocket requires a logical session"}
	}
	registry := e.credentialSockets
	if registry == nil {
		registry = &globalCodexCredentialWebsockets
	}
	key := strings.TrimSpace(auth.ID)
	if key == "" {
		apiKey, _ := codexCreds(auth)
		key = fmt.Sprintf("anonymous-%x", sha256.Sum256([]byte(apiKey)))
	}
	owner := helps.CodexOwnerFingerprint(auth)
	connectionID := uuid.NewString()
	target := helps.CodexWebsocketTarget{
		Credential: key, Owner: owner, URL: wsURL, Affinity: sess.sessionID,
		ExistingOnly: cliproxyexecutor.RequiredUpstreamWebsocket(ctx),
		Connected: func(*websocket.Conn) {
			log.WithFields(log.Fields{"connection": connectionID, "auth": auth.ID, "url": wsURL, "scope": "credential"}).Info("codex websockets: upstream connected")
		},
		Disconnected: func(_ *websocket.Conn, err error) {
			log.WithFields(log.Fields{"connection": connectionID, "auth": auth.ID, "url": wsURL, "scope": "credential", "reason": "transport_closed", "error": err}).Info("codex websockets: upstream disconnected")
		},
	}
	lane, handshake, err := registry.Acquire(ctx, target, func(dialCtx context.Context) (*websocket.Conn, *http.Response, error) {
		conn, _, response, errDial := e.dialCodexWebsocket(dialCtx, auth, wsURL, headers)
		if conn != nil {
			conn.SetPingHandler(func(payload string) error {
				return conn.WriteControl(websocket.PongMessage, []byte(payload), time.Time{})
			})
		}
		return conn, response, errDial
	})
	if err != nil {
		if errors.Is(err, helps.ErrCodexWebsocketConnectionRequired) {
			err = cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
		return nil, nil, handshake, err
	}
	if errContext := ctx.Err(); errContext != nil {
		lane.Release()
		return nil, nil, handshake, errContext
	}
	conn := lane.Conn()
	closer := &websocketConnectionCloser{conn: conn, closeFn: func() error { lane.Release(); return nil }}
	sess.connMu.Lock()
	previous := sess.credentialLane
	var previousLifecycle cliproxyexecutor.ExecutionLifecycle
	if sess.conn != conn {
		previousLifecycle, sess.lifecycle = sess.lifecycle, nil
		sess.lifecycleModel = ""
	}
	if previous == nil || previous.Conn() != conn || previous.StreamID() != lane.StreamID() || previous.Epoch() != lane.Epoch() {
		sess.continuation = &helps.CodexContinuation{}
		sess.multiAgentV2OptimizedConn = nil
	}
	watch := sess.readerConn != conn
	var watchCtx context.Context
	if watch {
		if sess.credentialWatchCancel != nil {
			sess.credentialWatchCancel()
		}
		watchCtx, sess.credentialWatchCancel = context.WithCancel(context.Background())
	}
	sess.conn, sess.connCloser = conn, closer
	sess.credentialLane, sess.credentialConn = lane, conn
	sess.authID, sess.wsURL = auth.ID, wsURL
	sess.proxyURL = executionProxyURL(ctx, e.cfg, auth)
	sess.readerConn = conn
	sess.connMu.Unlock()
	if previousLifecycle != nil {
		previousLifecycle.End("logical_target_changed")
	}
	if watch {
		sess.resetUpstreamDisconnectError(conn)
		go func() {
			select {
			case <-watchCtx.Done():
				return
			case <-lane.ConnectionDone():
			}
			errClose := lane.ConnectionError()
			sess.setUpstreamDisconnectError(conn, errClose)
			sess.connMu.Lock()
			var lifecycle cliproxyexecutor.ExecutionLifecycle
			matched := sess.conn == conn
			if matched {
				sess.conn, sess.connCloser, sess.readerConn = nil, nil, nil
				lifecycle, sess.lifecycle = sess.lifecycle, nil
				sess.lifecycleModel = ""
			}
			sess.connMu.Unlock()
			if matched {
				sess.notifyUpstreamDisconnect(errClose)
			}
			if lifecycle != nil {
				lifecycle.End("transport_closed")
			}
		}()
	}
	return conn, closer, handshake, nil
}

func (s *codexWebsocketSession) credentialLaneFor(conn *websocket.Conn) *helps.CodexWebsocketLane {
	if s == nil || conn == nil {
		return nil
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.credentialConn == conn {
		return s.credentialLane
	}
	return nil
}

// RetireExecutionSessions is called only when replacing an executor. Managed
// logical sessions and credential connections survive configuration hot reload.
func (e *CodexAutoExecutor) RetireExecutionSessions() {
	if e != nil && e.wsExec != nil {
		// Drain this executor's logical leases without touching the registry.
		e.poolOnce.Do(func() { e.pool = cliproxyexecutor.NewExecutionSessionPool(e.CloseExecutionSession) })
		e.pool.Drain()
		e.wsExec.retireLegacySessions()
	}
}

func (e *CodexWebsocketsExecutor) RetireExecutionSessions() {
	e.retireLegacySessions()
}

func (e *CodexWebsocketsExecutor) retireLegacySessions() {
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	var retired []*codexWebsocketSession
	for id, sess := range store.sessions {
		sess.connMu.Lock()
		managed := sess.credentialLane != nil
		sess.connMu.Unlock()
		if !managed {
			delete(store.sessions, id)
			retired = append(retired, sess)
		}
	}
	store.mu.Unlock()
	for _, sess := range retired {
		closeCodexWebsocketSession(sess, "executor_replaced")
	}
}

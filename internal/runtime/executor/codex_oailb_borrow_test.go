package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOaiLBHealthyWebsocketIgnoresCookieExpiryAndBorrowChanges(t *testing.T) {
	for _, topic := range []bool{false, true} {
		name := "session"
		if topic {
			name = "topic"
		}
		t.Run(name, func(t *testing.T) {
			var sourceCalls, connections atomic.Int32
			donor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sourceCalls.Add(1)
				_, _ = w.Write([]byte(`{"available":false}`))
			}))
			defer donor.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					kind, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						return
					}
					if errWrite := conn.WriteMessage(kind, payload); errWrite != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
			closeHTTPResponseBody(response, "test")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// Unexpected redial is confined to a local, unreachable proxy.
			proxy := "http://127.0.0.1:1"
			cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{
				SourceURL: donor.URL, SourceManagementKey: "key", SourceAuthID: "selected",
			}}}
			cfg.ProxyURL = proxy
			e := NewCodexWebsocketsExecutor(cfg)
			e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
			t.Cleanup(func() { e.CloseExecutionSession(auth.CloseAllExecutionSessionsID) })
			sessionID := t.Name()
			if topic {
				sessionID = helps.CodexTopicSessionPrefix + sessionID
			}
			sess := e.getOrCreateSession(sessionID)
			credential := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
			// Represent a completed ChatGPT handshake with a local socket. Retain
			// the real URL so a mistaken borrow refresh would access the test donor.
			u := "wss://chatgpt.com/backend-api/codex/responses"
			headers := make(http.Header)
			sess.conn, sess.connCloser = conn, newWebsocketConnectionCloser(conn)
			sess.authID, sess.wsURL, sess.proxyURL = credential.ID, u, proxy
			sess.connectionFingerprint = helps.CodexConnectionFingerprint(credential, headers, proxy)
			jwt := func(exp time.Time) string {
				payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
				return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
			}
			for _, state := range []string{"refresh-window", "expired", "source-changed", "disabled"} {
				switch state {
				case "refresh-window":
					headers.Set("Cookie", "__oailb="+jwt(time.Now().Add(time.Minute)))
				case "expired":
					headers.Set("Cookie", "__oailb="+jwt(time.Now().Add(-time.Hour)))
				case "source-changed":
					cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthID = "different-credential"
				case "disabled":
					cfg.CodexHeaderDefaults.OaiLBBorrow = nil
				}
				if existing, _ := existingWebsocketSessionConn(sess, credential.ID, u, proxy, helps.CodexConnectionFingerprint(credential, headers, proxy)); existing != conn {
					t.Fatalf("%s changed connection eligibility", state)
				}
				got, _, handshake, errEnsure := e.ensureUpstreamConn(ctx, credential, sess, credential.ID, u, headers)
				if errEnsure != nil || got != conn || handshake != nil {
					t.Fatalf("%s redialed: %v", state, errEnsure)
				}
				reads := sess.activate(conn)
				payload := []byte(`{"type":"response.create","input":[]}`)
				if errWrite := writeCodexWebsocketMessage(sess, conn, payload); errWrite != nil {
					t.Fatal(errWrite)
				}
				_, echoed, errRead := readCodexWebsocketMessage(ctx, sess, conn, reads)
				if errRead != nil || string(echoed) != string(payload) {
					t.Fatal("existing socket stopped carrying frames", errRead)
				}
				sess.clearActive(conn, reads)
			}
			if sourceCalls.Load() != 0 || connections.Load() != 1 {
				t.Fatalf("reuse fetched source %d times / opened %d sockets", sourceCalls.Load(), connections.Load())
			}
		})
	}
}

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

func TestOaiLBWebsocketHandshakeNodeSelection(t *testing.T) {
	for _, responseCookie := range []bool{false, true} {
		jwt := func(node string) string {
			body, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "host": "chat.gateway." + node + ".api.openai.com"})
			return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := make(http.Header)
			if responseCookie {
				h.Add("Set-Cookie", "__oailb="+jwt("unified-42")+"; Path=/")
			}
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, h)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, _, _ = c.ReadMessage()
		}))
		credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "key"}}
		e := NewCodexWebsocketsExecutor(&config.Config{})
		reporter := helps.NewUsageReporter(t.Context(), "codex", "model", credential)
		ctx := helps.WithCodexOaiLBReporter(t.Context(), reporter)
		conn, closer, resp, err := e.dialCodexWebsocketOnce(ctx, credential, "ws"+strings.TrimPrefix(upstream.URL, "http"), http.Header{"Cookie": {"__oailb=" + jwt("unified-96")}})
		closeHTTPResponseBody(resp, "test")
		if err != nil {
			upstream.Close()
			t.Fatal(err)
		}
		want := "unified-96"
		if responseCookie {
			want = "unified-42"
		}
		if reporter.OaiLBNode() != want || closer.oaiLBNode != want {
			t.Errorf("wrong handshake node: reporter=%q socket=%q", reporter.OaiLBNode(), closer.oaiLBNode)
		}
		_ = conn.Close()
		upstream.Close()
	}
}

func TestOaiLBReusedWebsocketKeepsHandshakeNode(t *testing.T) {
	for _, topic := range []bool{false, true} {
		name := "session"
		if topic {
			name = "topic"
		}
		t.Run(name, func(t *testing.T) {
			var connections atomic.Int32
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
			cfg := &config.Config{}
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
			// the real URL while all socket traffic remains on the local test server.
			u := "wss://chatgpt.com/backend-api/codex/responses"
			headers := make(http.Header)
			sess.conn, sess.connCloser = conn, newWebsocketConnectionCloser(conn)
			sess.authID, sess.wsURL, sess.proxyURL = credential.ID, u, proxy
			sess.connectionFingerprint = helps.CodexConnectionFingerprint(credential, headers, proxy)
			sess.connCloser.oaiLBNode = "unified-96"
			jwt := func(exp time.Time) string {
				payload, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
				return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
			}
			for _, state := range []string{"valid", "expired", "replaced", "missing"} {
				switch state {
				case "valid":
					headers.Set("Cookie", "__oailb="+jwt(time.Now().Add(time.Minute)))
				case "expired":
					headers.Set("Cookie", "__oailb="+jwt(time.Now().Add(-time.Hour)))
				case "replaced":
					headers.Set("Cookie", "__oailb="+jwt(time.Now().Add(time.Hour)))
				case "missing":
					headers.Del("Cookie")
				}
				if existing, _ := existingWebsocketSessionConn(sess, credential.ID, u, proxy, helps.CodexConnectionFingerprint(credential, headers, proxy)); existing != conn {
					t.Fatalf("%s changed connection eligibility", state)
				}
				reporter := helps.NewUsageReporter(ctx, "codex", "model", credential)
				requestCtx := helps.WithCodexOaiLBReporter(ctx, reporter)
				got, _, handshake, errEnsure := e.ensureUpstreamConn(requestCtx, credential, sess, credential.ID, u, headers)
				if reporter.OaiLBNode() != "unified-96" {
					t.Fatal("reused socket lost its handshake node")
				}
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
			if connections.Load() != 1 {
				t.Fatalf("reuse opened %d sockets", connections.Load())
			}
		})
	}
}

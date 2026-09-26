package executor

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTopicConnectionCompatibility(t *testing.T) {
	for _, change := range []string{"unchanged", "turn_and_tier", "token", "beta", "model_route", "client_headers", "custom_added", "custom_removed", "proxy"} {
		t.Run(change, func(t *testing.T) {
			var connections atomic.Int32
			var first atomic.Pointer[websocket.Conn]
			headersSeen := make(chan http.Header, 2)
			closedBeforeHandshake := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				if previous := first.Load(); previous != nil {
					// An empty write cannot inject a frame. A closed TCP connection
					// must reject it before the replacement handshake is accepted.
					_, err := previous.UnderlyingConn().Write(nil)
					closedBeforeHandshake <- err
				}
				headersSeen <- r.Header.Clone()
				peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = peer.Close() }()
				for {
					if _, _, err := peer.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
			e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
			defer e.CloseExecutionSession(auth.CloseAllExecutionSessionsID)
			credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "token-a", "account_id": "account"}}
			url := "ws" + strings.TrimPrefix(server.URL, "http")
			sess := e.getOrCreateSession(helps.CodexTopicSessionPrefix + t.Name())
			headers := http.Header{"Authorization": {"Bearer token-a"}, "Openai-Beta": {codexResponsesWebsocketBetaHeaderValue}, "X-Codex-Routing-Hint": {"model=gpt-6-astra"}}
			conn, _, _, err := e.ensureUpstreamConn(t.Context(), credential, sess, credential.ID, url, headers)
			if err != nil {
				t.Fatal(err)
			}
			first.Store(conn)
			<-headersSeen
			baseline := sess.continuationFor(conn)
			credential = credential.Clone()
			switch change {
			case "turn_and_tier":
				headers.Set("X-Codex-Turn-State", "new-state")
				headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"new-turn"}`)
				headers.Set("X-Client-Request-Id", "new-request")
				headers.Set("X-Codex-Routing-Hint", "model=gpt-6-astra;tier=priority")
			case "token":
				credential.Metadata["access_token"] = "token-b"
				headers.Set("Authorization", "Bearer token-b")
			case "beta":
				headers.Set("OpenAI-Beta", "responses_websockets=next")
			case "model_route":
				headers.Set("X-Codex-Routing-Hint", "model=gpt-6-luna")
			case "client_headers":
				for _, name := range []string{"User-Agent", "Originator", "Version", "X-Codex-Beta-Features"} {
					headers.Set(name, "new-value")
				}
			case "custom_added":
				headers.Set("X-Custom-Route", "operator-route")
			case "custom_removed":
				headers.Del("OpenAI-Beta")
				headers.Del("X-Codex-Routing-Hint")
			case "proxy":
				// Both routes reach the local server; the resolved route changed.
				credential.ProxyURL = "direct"
			}
			wantReplace := change == "token" || change == "proxy"
			proxyURL := executionProxyURL(t.Context(), e.cfg, credential)
			fingerprint := helps.CodexConnectionFingerprint(credential, headers, proxyURL)
			existing, _ := existingWebsocketSessionConn(sess, credential.ID, url, proxyURL, fingerprint)
			if (existing == nil) != wantReplace {
				t.Fatalf("required-connection lookup mismatch: got %p, replace=%t", existing, wantReplace)
			}
			// Competing dial attempts must all select one physical replacement.
			var workers sync.WaitGroup
			results := make(chan *websocket.Conn, 8)
			for range 8 {
				workers.Go(func() {
					next, _, _, err := e.ensureUpstreamConn(t.Context(), credential, sess, credential.ID, url, headers)
					if err != nil {
						t.Error(err)
					}
					results <- next
				})
			}
			workers.Wait()
			close(results)
			var selected *websocket.Conn
			for next := range results {
				if selected == nil {
					selected = next
				}
				if next == nil || next != selected || (next != conn) != wantReplace {
					t.Fatal("concurrent attempts did not select the same compatible connection")
				}
			}
			wantConnections := int32(1)
			if wantReplace {
				wantConnections = 2
				if err := <-closedBeforeHandshake; !errors.Is(err, net.ErrClosed) {
					t.Fatalf("old physical connection was not closed before replacement: %v", err)
				}
				got := <-headersSeen
				for _, name := range []string{"Authorization", "OpenAI-Beta", "X-Codex-Routing-Hint"} {
					if got.Get(name) != headers.Get(name) {
						t.Errorf("replacement did not use the current %s", name)
					}
				}
			}
			if connections.Load() != wantConnections {
				t.Fatalf("handshakes=%d, want %d", connections.Load(), wantConnections)
			}
			if (baseline != sess.continuationFor(selected)) != wantReplace {
				t.Fatal("continuation lifetime did not follow the physical connection")
			}
		})
	}
}

func TestCodexTopicLookupPreservesWireIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"native_explicit_cache", "native_no_cache", "header_only"} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				frames := make(chan []byte, 1)
				headersSeen := make(chan http.Header, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headersSeen <- r.Header.Clone()
					peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = peer.Close() }()
					_, frame, errRead := peer.ReadMessage()
					if errRead != nil {
						t.Error(errRead)
						return
					}
					frames <- frame
					_ = peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"result","output":[]}}`))
					_, _, _ = peer.ReadMessage()
				}))
				defer server.Close()
				e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
				e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
				defer e.CloseExecutionSession(auth.CloseAllExecutionSessionsID)
				credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "token", "account_id": "account", "codex_client_system": "windows"}}
				body := `{"model":"gpt-6-astra","input":[],"client_metadata":{"thread_id":"native-thread","session_id":"native-session","turn_id":"native-turn","x-codex-turn-metadata":"{\"session_id\":\"native-session\",\"thread_id\":\"native-thread\",\"turn_id\":\"native-turn\"}"}}`
				headers := make(http.Header)
				// Frozen output from the pre-topic baseline (69191389).
				wantCache := "ace475fb-5754-5e56-9327-eb799571b365"
				if kind == "native_explicit_cache" {
					body = strings.Replace(body, `"input":[]`, `"input":[],"prompt_cache_key":"native-cache"`, 1)
					wantCache = "native-cache"
				} else if kind == "header_only" {
					body = `{"model":"gpt-6-astra","input":[]}`
					headers.Set("Thread-Id", "native-thread")
					headers.Set("Session-Id", "native-session")
				}
				req := core.Request{Model: "gpt-6-astra", Payload: []byte(body), Metadata: map[string]any{core.ExecutionSessionMetadataKey: "old-execution-session"}}
				opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Headers: headers, Metadata: map[string]any{core.ExecutionSessionMetadataKey: "old-execution-session"}}
				topic := e.websocketSessionID(credential, req, opts, "ws"+strings.TrimPrefix(server.URL, "http")+"/responses")
				if !strings.HasPrefix(topic, helps.CodexTopicSessionPrefix) {
					t.Fatal("fixture did not resolve a stable topic")
				}
				ctx := core.WithCodexTransport(core.WithDownstreamWebsocket(t.Context()), &core.CodexTransportState{})
				if stream {
					result, err := e.ExecuteStream(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(ctx, credential, req, opts); err != nil {
					t.Fatal(err)
				}
				frame, wireHeaders := <-frames, <-headersSeen
				if got := gjson.GetBytes(frame, "prompt_cache_key").String(); got != wantCache {
					t.Fatalf("topic lookup changed wire cache identity: %q, want %q", got, wantCache)
				}
				sessionHeader := "Session-Id"
				wantSession := wantCache
				if kind == "header_only" {
					wantSession = "native-session"
					if wireHeaders.Get("Conversation_id") != wantCache {
						t.Fatal("topic lookup changed the generated conversation header")
					}
				} else {
					for key, want := range map[string]string{"thread_id": "native-thread", "session_id": "native-session", "turn_id": "native-turn"} {
						if gjson.GetBytes(frame, "client_metadata."+key).String() != want {
							t.Errorf("caller %s changed", key)
						}
					}
				}
				if wireHeaders.Get(sessionHeader) != wantSession {
					t.Fatalf("handshake %s=%q, want %q", sessionHeader, wireHeaders.Get(sessionHeader), wantSession)
				}
				if req.Metadata[core.ExecutionSessionMetadataKey] != "old-execution-session" || opts.Metadata[core.ExecutionSessionMetadataKey] != "old-execution-session" {
					t.Fatal("topic lookup mutated execution metadata")
				}
				if strings.Contains(string(frame), helps.CodexTopicSessionPrefix) {
					t.Fatal("internal topic key leaked onto the wire")
				}
			})
		}
	}
}

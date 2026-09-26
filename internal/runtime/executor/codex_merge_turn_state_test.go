package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	authpkg "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Upstream withholds all completions until three creates arrive on the same
// socket. Each create must use the metadata received during the prior response.
func TestCodexDuplexTurnStateReusedBeforeCompletion(t *testing.T) {
	for _, ticketEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("ticket=%v", ticketEnabled), func(t *testing.T) {
			model := "gpt-6-astra"
			length := 312
			if ticketEnabled {
				length = 780
			}
			serverErrors := make(chan error, 1)
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					serverErrors <- err
					return
				}
				defer func() { _ = conn.Close() }()
				for i := 0; i < 3; i++ {
					_, body, errRead := conn.ReadMessage()
					if errRead != nil {
						serverErrors <- errRead
						return
					}
					if ticketEnabled {
						assertCodexReservedWireFixtures(t, body)
						if !gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Bool() {
							t.Error("duplex create lost Lite mirror")
						}
					}
					if i > 0 {
						want := strings.Repeat(string(rune('a'+i-1)), length)
						if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != want {
							serverErrors <- fmt.Errorf("create %d state len=%d, want latest %d-byte state", i+1, len(got), length)
							return
						}
					}
					created := fmt.Sprintf(`{"type":"response.created","response":{"id":"r%d","output":[]}}`, i+1)
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(created)); errWrite != nil {
						serverErrors <- errWrite
						return
					}
					if i < 2 {
						metadata, _ := json.Marshal(map[string]any{"type": "codex.response.metadata", "response_id": fmt.Sprintf("r%d", i+1), "headers": map[string]string{"x-codex-turn-state": strings.Repeat(string(rune('a'+i)), length)}})
						if errWrite := conn.WriteMessage(websocket.TextMessage, metadata); errWrite != nil {
							serverErrors <- errWrite
							return
						}
					}
				}
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r3","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); errWrite != nil {
					serverErrors <- errWrite
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			input := make(chan core.WebsocketInput, 2)
			ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)
			cfg := &config.Config{Codex: config.CodexConfig{ResponseSteering: true, TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: ticketEnabled}}}
			auth := &authpkg.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL, authpkg.AttributeAuthKind: authpkg.AuthKindOAuth}, Metadata: map[string]any{"access_token": "test-token"}}
			t.Cleanup(func() { helps.InvalidateCodexTurnStates(auth.ID); helps.InvalidateCodexCookieJar(auth.ID) })
			exec := NewCodexWebsocketsExecutor(cfg)
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			payload := func(i int) []byte {
				turn := "turn-1"
				if ticketEnabled {
					turn = fmt.Sprintf("turn-%d", i)
				}
				if ticketEnabled {
					turnMetadata := fmt.Sprintf(`{"turn_id":%q,"session_id":"duplex-session"}`, turn)
					return []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[],"tools":[{"type":"web_search"}],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true","x-codex-turn-metadata":%q}}`, model, turnMetadata))
				}
				return []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[],"client_metadata":{"turn_id":%q}}`, model, turn))
			}
			result, err := exec.ExecuteStream(ctx, auth, core.Request{Model: model, Payload: payload(1)}, core.Options{SourceFormat: translator.FormatCodex})
			if err != nil {
				t.Fatal(err)
			}
			metadataCount := 0
			for {
				select {
				case errServer := <-serverErrors:
					t.Fatal(errServer)
				case <-ctx.Done():
					t.Fatal("state reuse stalled before response completion: ", ctx.Err())
				case chunk, ok := <-result.Chunks:
					if !ok {
						t.Fatal("stream closed before third response")
					}
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					switch gjson.GetBytes(chunk.Payload, "type").String() {
					case "codex.response.metadata":
						metadataCount++
						input <- core.WebsocketInput{Payload: payload(metadataCount + 1)}
					case "response.completed":
						if metadataCount != 2 {
							t.Fatalf("metadata count=%d", metadataCount)
						}
						cancel()
						for range result.Chunks {
						}
						<-serverDone
						return
					}
				}
			}
		})
	}
}

func TestCodexConnectionReuseRequiresProxyAndIdentity(t *testing.T) {
	conn := &websocket.Conn{}
	auth := &authpkg.Auth{ID: "a", Provider: "codex", ProxyURL: "http://credential-proxy", Metadata: map[string]any{"access_token": "test-token"}}
	headers := http.Header{"User-Agent": {"codex-tui/0.156.0"}}
	proxy := "http://request-proxy"
	fingerprint := helps.CodexConnectionFingerprint(auth, headers, proxy)
	if fingerprint == helps.CodexConnectionFingerprint(auth, headers, auth.ProxyURL) {
		t.Fatal("request proxy ignored in connection fingerprint")
	}
	sess := &codexWebsocketSession{authID: auth.ID, wsURL: "wss://example.test/responses", proxyURL: proxy, connectionFingerprint: fingerprint, conn: conn, connCloser: newWebsocketConnectionCloser(conn)}
	if got, _ := existingWebsocketSessionConn(sess, auth.ID, sess.wsURL, proxy, fingerprint); got != conn {
		t.Fatal("same target was not reused")
	}
	if got, _ := existingWebsocketSessionConn(sess, auth.ID, sess.wsURL, auth.ProxyURL, fingerprint); got != nil {
		t.Fatal("wrong proxy reused")
	}
	headers.Set("User-Agent", "codex-tui/changed")
	if got, _ := existingWebsocketSessionConn(sess, auth.ID, sess.wsURL, proxy, helps.CodexConnectionFingerprint(auth, headers, proxy)); got != conn {
		t.Fatal("client version change prevented reuse")
	}
}

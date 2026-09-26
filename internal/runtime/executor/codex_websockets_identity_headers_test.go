package executor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketIdentityHeaderPriorityAndFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"explicit", "gin_headers", "legacy_alias", "thread_header", "body_only", "cache_only", "child", "no_ids_execution", "no_ids_derived", "no_ids_native_random", "no_ids_compat", "configured"} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				headersSeen := make(chan http.Header, 1)
				frames := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headersSeen <- r.Header.Clone()
					peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = peer.Close() }()
					_, frame, err := peer.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					frames <- frame
					_ = peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"result","output":[]}}`))
				}))
				defer server.Close()
				e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
				e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
				defer e.CloseExecutionSession(auth.CloseAllExecutionSessionsID)
				credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token", "account_id": "account", "codex_client_system": "windows"}}
				ctx := core.WithDownstreamWebsocket(t.Context())
				body := `{"model":"gpt-6-astra","input":[],"prompt_cache_key":"body-topic","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"body-topic\",\"thread_id\":\"body-topic\",\"turn_id\":\"turn\"}"}}`
				headers := make(http.Header)
				metadata := make(map[string]any)
				wantSession, wantThread, wantRequest, wantCache := "body-topic", "body-topic", "body-topic", "body-topic"
				switch kind {
				case "explicit", "gin_headers", "legacy_alias", "configured":
					headers = http.Header{"Session-Id": {"client-session"}, "Thread-Id": {"client-thread"}, "X-Client-Request-Id": {"client-request"}}
					wantSession, wantThread, wantRequest = "client-session", "client-thread", "client-request"
					if kind == "gin_headers" {
						ctx = core.WithDownstreamWebsocket(contextWithGinHeaders(map[string]string{"Session-Id": "client-session", "Thread-Id": "client-thread", "X-Client-Request-Id": "client-request"}))
						headers = nil
					} else if kind == "legacy_alias" {
						headers = http.Header{"session_id": {"client-session"}, "thread_id": {"client-thread"}, "x-client-request-id": {"client-request"}}
					} else if kind == "configured" {
						for _, key := range []string{"Session-Id", "Thread-Id", "X-Client-Request-Id"} {
							credential.Attributes["header:"+key] = "configured"
						}
						wantSession, wantThread, wantRequest = "configured", "configured", "configured"
					}
				case "thread_header":
					headers.Set("Thread-Id", "client-thread")
					wantThread, wantRequest = "client-thread", "client-thread"
				case "cache_only":
					body = `{"model":"gpt-6-astra","input":[],"prompt_cache_key":"body-topic","client_metadata":{"x-codex-turn-metadata":"{}"}}`
				case "child":
					body = `{"model":"gpt-6-astra","input":[],"prompt_cache_key":"body-topic","client_metadata":{"x-openai-subagent":"worker","x-codex-turn-metadata":"{\"session_id\":\"child\",\"thread_id\":\"child\",\"turn_id\":\"child-turn\"}"}}`
					wantSession, wantThread, wantRequest = "child", "child", "child"
				case "no_ids_execution", "no_ids_derived", "no_ids_compat":
					body = `{"model":"gpt-6-astra","input":[]}`
					if kind == "no_ids_execution" {
						metadata[core.ExecutionSessionMetadataKey] = "existing-execution"
					} else if kind == "no_ids_derived" {
						metadata[core.DerivedSessionIDMetadataKey] = "ctx:v1:existing-root"
					}
					wantCache = helps.ProviderSessionUUID("codex", metadata)
					wantSession, wantThread, wantRequest = wantCache, wantCache, wantCache
				case "no_ids_native_random":
					body = `{"model":"gpt-6-astra","input":[],"client_metadata":{"x-codex-turn-metadata":"{}"}}`
				}
				req := core.Request{Model: "gpt-6-astra", Payload: []byte(body), Metadata: metadata}
				opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Headers: headers, Metadata: metadata}
				originalHeaders := headers.Clone()
				// Incoming identity, not generated fallback IDs, determines topic ownership.
				if kind == "no_ids_execution" || kind == "no_ids_derived" || kind == "no_ids_compat" || kind == "no_ids_native_random" {
					wantOwner, _ := metadata[core.ExecutionSessionMetadataKey].(string)
					if got := e.websocketSessionID(credential, req, opts, server.URL); got != wantOwner {
						t.Fatalf("missing identity bypassed legacy execution ownership: %q", got)
					}
				}
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
				frame, gotHeaders := <-frames, <-headersSeen
				if kind == "no_ids_native_random" {
					wantCache = gjson.GetBytes(frame, "prompt_cache_key").String()
					if _, err := uuid.Parse(wantCache); err != nil {
						t.Fatal("existing native UUID fallback was lost")
					}
					wantSession, wantThread, wantRequest = wantCache, wantCache, wantCache
				}
				for key, want := range map[string]string{"Session-Id": wantSession, "Thread-Id": wantThread, "X-Client-Request-Id": wantRequest} {
					if got := gotHeaders.Get(key); got != want {
						t.Errorf("%s=%q, want %q", key, got, want)
					}
				}
				if got := gjson.GetBytes(frame, "prompt_cache_key").String(); got != wantCache {
					t.Errorf("header restoration changed body cache identity: %q, want %q", got, wantCache)
				}
				if gotHeaders.Get("Session_id") != "" || gotHeaders.Get("Thread_id") != "" {
					t.Fatal("duplicate legacy identity header on the wire")
				}
				if !reflect.DeepEqual(headers, originalHeaders) || string(req.Payload) != body {
					t.Fatal("identity restoration mutated the caller request")
				}
			})
		}
	}
}

package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	config "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtime "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketTopicSurvivesDownstreamReconnect(t *testing.T) {
	for _, duplex := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplex=%t", duplex), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			var connections atomic.Int32
			observed := make(chan int32, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				number := connections.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					_, body, errRead := conn.ReadMessage()
					if errRead != nil {
						return
					}
					observed <- number
					if gjson.GetBytes(body, "type").String() != "response.create" {
						t.Errorf("unexpected request: %s", body)
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"upstream-state"}}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"upstream-response","output":[]}}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"upstream-response","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
				}
			}))
			defer upstream.Close()
			cfg := &config.Config{Codex: config.CodexConfig{ForceWebsocket: true, ResponseSteering: duplex}, SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			executor := runtime.NewCodexWebsocketsExecutor(cfg)
			manager.RegisterExecutor(executor)
			t.Cleanup(func() { executor.CloseExecutionSession(auth.CloseAllExecutionSessionsID) })
			credential := &auth.Auth{ID: t.Name(), Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL, "api_key": "test-key", "websockets": "true"}}
			if _, err := manager.Register(t.Context(), credential); err != nil {
				t.Fatal(err)
			}
			model := fmt.Sprintf("topic-reconnect-%t", duplex)
			registry.GetGlobalRegistry().RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexForceWebsocket: true, CodexResponseSteering: duplex}, manager))
			ended := make(chan struct{}, 4)
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) { defer func() { ended <- struct{}{} }(); handler.ResponsesWebsocket(c) })
			downstream := httptest.NewServer(router)
			defer downstream.Close()
			dial := func(headers http.Header) *websocket.Conn {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", headers)
				if err != nil {
					t.Fatal(err)
				}
				// Test failure bound only; production topic connections add no deadline.
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				return conn
			}
			send := func(conn *websocket.Conn, metadata string) int32 {
				t.Helper()
				body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":"hello"}],"prompt_cache_key":"shared-cache"%s}`, model, metadata))
				if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
					t.Fatal(err)
				}
				for {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					kind := gjson.GetBytes(payload, "type").String()
					if kind == "error" {
						t.Fatalf("websocket error: %s", payload)
					}
					if kind == "response.completed" {
						break
					}
				}
				return <-observed
			}
			first := dial(nil)
			if n := send(first, `,"client_metadata":{"thread_id":"root","turn_id":"turn-1"}`); n != 1 {
				t.Fatal(n)
			}
			if n := send(first, `,"client_metadata":{"thread_id":"root","turn_id":"turn-2"}`); n != 1 {
				t.Fatal(n)
			}
			_ = first.Close()
			<-ended
			// A different downstream socket/header spelling must find the same topic.
			second := dial(http.Header{"Thread-Id": {"root"}})
			if n := send(second, ""); n != 1 {
				t.Fatalf("downstream reconnect replaced upstream: %d", n)
			}
			child := dial(http.Header{"Thread-Id": {"child"}, "Session-Id": {"root"}, "X-Codex-Parent-Thread-Id": {"root"}})
			if n := send(child, `,"client_metadata":{"thread_id":"child","session_id":"root","turn_id":"child-turn"}`); n != 2 {
				t.Fatalf("child shared parent socket: %d", n)
			}
			_ = child.Close()
			<-ended
			_ = second.Close()
			<-ended
			if n := connections.Load(); n != 2 {
				t.Fatalf("upstream connections=%d", n)
			}
		})
	}
}

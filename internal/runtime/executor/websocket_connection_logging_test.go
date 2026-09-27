package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestWebsocketConnectionLogsUseAccessLogSession(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })
	for _, provider := range []string{"codex", "basispoints"} {
		for _, downstream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/downstream_ws=%t", provider, downstream), func(t *testing.T) {
				hook.Reset()
				var connections atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					connections.Add(1)
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-log","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
					}
				}))
				defer server.Close()
				cfg := forceWebsocketTestConfig()
				cfg.RequestLog = false
				auth := basispointsTestAuth()
				auth.ID = t.Name()
				auth.Attributes = map[string]string{"base_url": server.URL}
				native := NewCodexWebsocketsExecutor(cfg)
				native.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				defer native.CloseExecutionSession(coreauth.CloseAllExecutionSessionsID)
				bps := NewBasispointsExecutor(cfg)
				bps.websocketDial = basispointsTestDial(server.URL)
				const session = "01a0d8fc-1111-2222-3333-444444444444"
				router := gin.New()
				router.Use(logging.GinLogrusLogger())
				router.Any("/v1/responses", func(c *gin.Context) {
					ctx := context.WithValue(c.Request.Context(), "gin", c)
					if downstream {
						ctx = core.WithDownstreamWebsocket(ctx)
					}
					req := core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[],"client_metadata":{"session_id":"` + session + `","turn_id":"01a0d8fd-1111-2222-3333-444444444444"}}`)}
					opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Headers: c.Request.Header}
					var result *core.StreamResult
					var err error
					if provider == "codex" {
						result, err = native.ExecuteStream(ctx, auth, req, opts)
					} else {
						result, err = bps.ExecuteStream(ctx, auth, req, opts)
					}
					if err != nil {
						t.Error(err)
						c.Status(500)
						return
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Error(chunk.Err)
						}
						_, _ = c.Writer.Write(chunk.Payload)
					}
				})
				for range 2 {
					method := http.MethodPost
					if downstream {
						method = http.MethodGet
					}
					req := httptest.NewRequest(method, "/v1/responses", nil)
					req.Header.Set("Session-Id", session)
					if downstream {
						req.Header.Set("Connection", "Upgrade")
						req.Header.Set("Upgrade", "websocket")
					}
					router.ServeHTTP(httptest.NewRecorder(), req)
					if req.Method != method {
						t.Fatal("logging mutated the HTTP request method")
					}
				}
				native.CloseExecutionSession(coreauth.CloseAllExecutionSessionsID)
				connected, disconnected, access := 0, 0, 0
				wantMethod := "POST/WS"
				if downstream {
					wantMethod = "WS/WS"
				}
				for _, entry := range hook.AllEntries() {
					connection := strings.Contains(entry.Message, "websockets: upstream connected") || strings.Contains(entry.Message, "websockets: upstream disconnected")
					isAccess := strings.Contains(entry.Message, `"/v1/responses"`)
					if connection || isAccess {
						if entry.Data["session_id"] != "01a0d8fc" {
							t.Fatalf("unrelated log session: %s %v", entry.Message, entry.Data)
						}
						formatted, err := (&logging.LogFormatter{}).Format(entry)
						if err != nil || !strings.Contains(string(formatted), "[01a0d8fc]") || strings.Contains(string(formatted), "codex-topic/") || strings.Contains(string(formatted), "connection=") {
							t.Fatalf("unrelated identifier in formatted log: %s (%v)", formatted, err)
						}
					}
					if strings.Contains(entry.Message, "websockets: upstream connected") {
						connected++
					}
					if strings.Contains(entry.Message, "websockets: upstream disconnected") {
						disconnected++
					}
					if isAccess {
						access++
						if !strings.Contains(entry.Message, wantMethod) {
							t.Fatalf("wrong transport label: %s", entry.Message)
						}
					}
				}
				wantConnections := 1
				if provider == "basispoints" {
					wantConnections = 2
				}
				if int(connections.Load()) != wantConnections || connected != wantConnections || disconnected != wantConnections || access != 2 {
					t.Fatalf("connections=%d connected=%d disconnected=%d access=%d", connections.Load(), connected, disconnected, access)
				}
			})
		}
	}
}

func TestWebsocketHandshakeAndFallbackAccessLabels(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })
	for _, requestLog := range []bool{false, true} {
		for _, fallback := range []bool{false, true} {
			hook.Reset()
			cfg := forceWebsocketTestConfig()
			cfg.RequestLog = requestLog
			router := gin.New()
			router.Use(logging.GinLogrusLogger())
			router.POST("/v1/responses", func(c *gin.Context) {
				ctx := context.WithValue(c.Request.Context(), "gin", c)
				helps.RecordAPIWebsocketUpgradeRejection(ctx, cfg, helps.UpstreamRequestLog{
					URL: "https://chatgpt.com/backend-api/codex/responses", Method: http.MethodGet,
					Headers: http.Header{"Upgrade": {"websocket"}},
				}, http.StatusUpgradeRequired, http.Header{}, nil)
				if fallback {
					helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{URL: "https://chatgpt.com/backend-api/codex/responses", Method: http.MethodPost})
				}
				c.Status(200)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			entry := hook.LastEntry()
			if entry == nil || strings.Contains(entry.Message, "POST/WS") == fallback {
				t.Fatalf("request_log=%t fallback=%t: %v", requestLog, fallback, entry)
			}
		}
	}
}

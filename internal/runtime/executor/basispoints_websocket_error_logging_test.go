package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestBasispointsWebsocketToolErrorsLogBeforeFinalize(t *testing.T) {
	for _, requestLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("logging=%t", requestLog), func(t *testing.T) {
			codes := []string{
				`{"name":"shell","arguments":{"cmd":"echo" "broken"}}`,
				`{"name":"undeclared_tool","arguments":{}}`,
			}
			var connections atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				connections.Add(1)
				defer func() { _ = conn.Close() }()
				for i := 0; ; i++ {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
					output := []any{}
					if i < len(codes) {
						args, _ := json.Marshal(map[string]any{"code": codes[i], "summary": "Invoke tool"})
						output = append(output, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", i), "call_id": fmt.Sprintf("call_%d", i), "name": "run_officejs", "arguments": string(args)})
					}
					err = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", i), "status": "completed", "output": output}})
					if err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			cfg := &config.Config{Codex: config.CodexConfig{ForceWebsocket: true, Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
			cfg.RequestLog, cfg.CodexBasispoints = requestLog, true
			exec := NewCodexAutoExecutor(cfg)
			exec.basispointsExec.cooldown = basispoints.NewCooldown(time.Now)
			isolateBasispointsWebsockets(t, exec.basispointsExec)
			exec.basispointsExec.websocketDial = basispointsTestDial(upstream.URL)
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(exec)
			auth := basispointsTestAuth()
			auth.ID = t.Name()
			auth.Metadata["disable_cooling"] = true
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-6-sol"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
			dir := t.TempDir()
			logger := logging.NewFileRequestLogger(requestLog, dir, "", 10)
			requestIDs := make(chan string, 1)
			finished := make(chan struct{}, 3)
			releaseFinalize := make(chan struct{})
			router := gin.New()
			router.Use(func(c *gin.Context) { defer func() { finished <- struct{}{} }(); c.Next() })
			router.Use(logging.GinLogrusLogger(), middleware.RequestLoggingMiddleware(logger))
			router.GET("/v1/responses", func(c *gin.Context) {
				requestIDs <- logging.GetGinRequestID(c)
				h.ResponsesWebsocket(c)
				<-releaseFinalize
			})
			server := httptest.NewServer(router)
			defer server.Close()
			runTurn := func(i int, errorCode string) {
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"tool-error-session"}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close(); releaseFinalize <- struct{}{}; <-finished }()
				requestID := <-requestIDs
				if requestID == "" {
					t.Fatal("missing request ID")
				}
				payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-6-sol","input":[{"role":"user","content":"diagnostic-turn-%d"}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`, i)
				if err = conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
					t.Fatal(err)
				}
				_, event, errRead := conn.ReadMessage()
				if errorCode == "" {
					if errRead != nil {
						t.Fatal(errRead)
					}
					if gjson.GetBytes(event, "type").String() != "response.completed" {
						t.Fatalf("connection not reusable: %s", event)
					}
					return
				}
				if errRead == nil && !bytes.Contains(event, []byte(errorCode)) {
					t.Fatalf("wrong failure: %s", event)
				}
				files, errGlob := filepath.Glob(filepath.Join(dir, "*-"+logging.ShortRequestID(requestID)+".log"))
				if errGlob != nil || len(files) != 1 {
					t.Errorf("error log unavailable before middleware finalization: files=%v err=%v", files, errGlob)
					return
				}
				var content []byte
				for _, file := range files {
					part, errRead := os.ReadFile(file)
					if errRead != nil {
						t.Fatal(errRead)
					}
					content = append(content, part...)
				}
				for _, wanted := range []string{errorCode, codes[i], fmt.Sprintf("diagnostic-turn-%d", i), "wss://bps.openai.com/basispoints/api/responses"} {
					if !bytes.Contains(content, []byte(wanted)) {
						t.Errorf("error log lacks %q", wanted)
					}
				}
				if bytes.Contains(content, []byte(auth.Metadata["access_token"].(string))) {
					t.Fatal("error log contains bearer token")
				}
			}
			for i, errorCode := range []string{"invalid_tool_envelope", "unexpected_tool", ""} {
				runTurn(i, errorCode)
			}
			if connections.Load() != 1 {
				t.Fatalf("tool errors broke websocket reuse: connections=%d", connections.Load())
			}
		})
	}
}

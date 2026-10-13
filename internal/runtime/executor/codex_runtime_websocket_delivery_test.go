package executor

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexRuntimeWebsocketNextRequestDoesNotWaitForDiagnostics(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	settings, auth := runtimeTestMaster(t, "websocket-logs.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
		call := calls.Add(1)
		runtimeLogNotification(conn, req, runtimeLogRequest())
		runtimeTestAccept(conn, rpc, req)
		_ = conn.WriteJSON(map[string]any{"method": "cpa/inference/event", "params": map[string]any{"requestId": req.RequestID, "bodyBase64": []byte("data: " + runtimeTerminal + "\n\n")}})
		_ = conn.WriteJSON(map[string]any{"method": "cpa/inference/completed", "params": map[string]any{"requestId": req.RequestID}})
		if call == 1 {
			<-release
		}
		runtimeLogNotification(conn, req, bridge.UpstreamLog{Kind: "response", StatusCode: 200})
		_ = conn.WriteJSON(map[string]any{"method": "cpa/inference/diagnostics/completed", "params": map[string]any{"requestId": req.RequestID}})
	})
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
	cfg.RequestLog = true
	runtimeImageAuth(t, cfg, auth)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	handler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	logs := make(chan string, 1)
	router.Use(func(c *gin.Context) {
		c.Next()
		value, _ := c.Get("API_RESPONSE")
		body, _ := value.([]byte)
		logs <- string(body)
	})
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for index := range 2 {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"runtime-model","input":[{"role":"user","content":"independent request"}]}`)); err != nil {
			t.Fatal(err)
		}
		_, body, err := conn.ReadMessage()
		if err != nil || gjson.GetBytes(body, "type").String() != "response.completed" {
			t.Fatalf("request %d waited for previous diagnostics: %s (%v)", index+1, body, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("next request was not sent before log release: %d", calls.Load())
	}
	unblock()
	_ = conn.Close()
	select {
	case text := <-logs:
		if strings.Count(text, "=== API RESPONSE ") != 2 {
			t.Fatalf("WebSocket cleanup lost diagnostic exchanges: %s", text)
		}
	case <-ctx.Done():
		t.Fatal("WebSocket diagnostic cleanup did not finish", ctx.Err())
	}
}

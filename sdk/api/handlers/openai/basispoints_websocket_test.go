package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestBasispointsWebsocketReplaysHistoryOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &websocketDirectCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "basispoints-bridge-test"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexBasispoints: true, CodexResponseSteering: true}, manager))
	if h.websocketAuthEnabled(auth) || h.websocketUpstreamSupportsIncrementalInputForModel("basispoints-bridge-test") {
		t.Fatal("Basispoints selected native upstream WebSocket")
	}
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, request := range []string{
		`{"type":"response.create","model":"basispoints-bridge-test","input":[{"role":"user","content":"first"}]}`,
		`{"type":"response.create","previous_response_id":"resp-1","input":[{"role":"user","content":"second"}]}`,
	} {
		if err = conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatal(err)
		}
		_, response, errRead := conn.ReadMessage()
		if errRead != nil || gjson.GetBytes(response, "type").String() != "response.completed" {
			t.Fatalf("response=%s err=%v", response, errRead)
		}
	}
	payloads := executor.Payloads()
	if len(payloads) != 2 || gjson.GetBytes(payloads[1], "previous_response_id").Exists() || len(gjson.GetBytes(payloads[1], "input").Array()) != 3 {
		t.Fatalf("missing full history: %q", payloads)
	}
}

package openai

import (
	"fmt"
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
	"github.com/tidwall/sjson"
)

func TestForcedWebsocketWarmupRootAndImageToolException(t *testing.T) {
	const imageTool = `{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","parameters":{"type":"object"}}]}`
	for _, tc := range []struct {
		name, oldTool, newTool string
		want                   int
	}{
		{"changed ordinary tools", `{"type":"namespace","name":"functions","tools":[]}`, `{"type":"namespace","name":"mcp__cua_repl","tools":[]}`, 7},
		{"image tool in warmup", imageTool, `{"type":"namespace","name":"functions","tools":[]}`, 8},
		{"image tool in generation", `{"type":"namespace","name":"functions","tools":[]}`, imageTool, 8},
		{"mention only", `{"type":"function","name":"exec","description":"image_gen is mentioned here"}`, `{"type":"function","name":"exec"}`, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warmup := []byte(fmt.Sprintf(`{"model":"force-bridge-model","generate":false,"input":[{"type":"additional_tools","id":"old-tools","role":"developer","tools":[%s]},{"type":"message","id":"base","role":"developer","content":"base"}]}`, tc.oldTool))
			items := []string{fmt.Sprintf(`{"type":"additional_tools","id":"new-tools","role":"developer","tools":[%s]}`, tc.newTool), `{"type":"message","id":"base","role":"developer","content":"base"}`}
			for i := range 5 {
				items = append(items, fmt.Sprintf(`{"type":"message","id":"new-%d","role":"user","content":"image_gen in prompt text"}`, i))
			}
			root := []byte(`{"type":"response.create","input":[` + strings.Join(items, ",") + `]}`)
			got, retained, errMsg := normalizeResponsesWebsocketForcedRequest(root, warmup, []byte(`[]`), "warmup-response", nil, true)
			if errMsg != nil {
				t.Fatal(errMsg)
			}
			if len(gjson.GetBytes(got, "input").Array()) != tc.want || string(retained) != string(got) {
				t.Fatalf("root/retained input: %s / %s", got, retained)
			}
			if gjson.GetBytes(got, "previous_response_id").Exists() || gjson.GetBytes(got, "generate").Exists() || gjson.GetBytes(got, "model").String() != "force-bridge-model" {
				t.Fatalf("warmup state leaked: %s", got)
			}
			// A normal later root never inherits a completed generation, even with image tools.
			got, _, errMsg = normalizeResponsesWebsocketForcedRequest(root, retained, []byte(`[]`), "generation-response", nil, true)
			if errMsg != nil || len(gjson.GetBytes(got, "input").Array()) != 7 {
				t.Fatalf("later root inherited stale input: %s / %v", got, errMsg)
			}
			// Explicit parent continuations retain a full replay for fallback.
			delta := []byte(`{"type":"response.create","previous_response_id":"generation-response","input":[{"type":"message","id":"delta","role":"user","content":"next"}]}`)
			got, _, errMsg = normalizeResponsesWebsocketForcedRequest(delta, retained, []byte(`[]`), "generation-response", nil, true)
			if errMsg != nil || len(gjson.GetBytes(got, "input").Array()) != tc.want+1 || gjson.GetBytes(got, "previous_response_id").Exists() {
				t.Fatalf("delta lost replay: %s / %v", got, errMsg)
			}
			invalid, _ := sjson.SetBytes(root, "input", "not-an-array")
			if _, _, errMsg = normalizeResponsesWebsocketForcedRequest(invalid, warmup, nil, "", nil, true); errMsg == nil {
				t.Fatal("invalid root was accepted")
			}
		})
	}
}

func TestForcedWebsocketBridgeRetainsFullReplayAndValidatesParent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &websocketDirectCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "force-bridge-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexForceWebsocket: true}, manager))
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
		`{"type":"response.create","model":"force-bridge-model","input":[{"type":"message","id":"user-1","role":"user","content":"first"}]}`,
		`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"user-2","role":"user","content":"second"}]}`,
	} {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
			t.Fatal(errWrite)
		}
		_, response, errRead := conn.ReadMessage()
		if errRead != nil || gjson.GetBytes(response, "type").String() != "response.completed" {
			t.Fatalf("response=%s err=%v", response, errRead)
		}
	}
	payloads := executor.Payloads()
	if len(payloads) != 2 || gjson.GetBytes(payloads[1], "previous_response_id").Exists() || len(gjson.GetBytes(payloads[1], "input").Array()) != 3 {
		t.Fatalf("lost full replay: %q", payloads)
	}
	badParent := []byte(`{"type":"response.create","previous_response_id":"unknown-parent","input":[]}`)
	if errWrite := conn.WriteMessage(websocket.TextMessage, badParent); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, response, errRead := conn.ReadMessage()
	if errRead != nil || !strings.Contains(string(response), "previous_response") || len(executor.Payloads()) != 2 {
		t.Fatalf("unknown parent executed: %s %v", response, errRead)
	}
	root := []byte(`{"type":"response.create","input":[{"type":"message","id":"fresh-root","role":"user","content":"restart"}]}`)
	if errWrite := conn.WriteMessage(websocket.TextMessage, root); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, response, errRead = conn.ReadMessage()
	payloads = executor.Payloads()
	if errRead != nil || gjson.GetBytes(response, "type").String() != "response.completed" || len(payloads) != 3 || len(gjson.GetBytes(payloads[2], "input").Array()) != 1 {
		t.Fatalf("root retained stale history: payloads=%q response=%s err=%v", payloads, response, errRead)
	}
}

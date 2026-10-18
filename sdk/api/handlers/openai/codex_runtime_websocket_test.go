package openai

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type runtimeWebsocketCaptureExecutor struct {
	websocketDirectCaptureExecutor
	websocket bool
}

func (e *runtimeWebsocketCaptureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	coreexecutor.ReportUpstreamWebsocket(ctx, e.websocket)
	return e.websocketDirectCaptureExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestCodexRuntimeWebsocketMaterializesClientContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, upstreamWS := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			for _, steering := range []bool{false, true} {
				t.Run(fmt.Sprintf("websocket=%t/force=%t/steering=%t", upstreamWS, force, steering), func(t *testing.T) {
					cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
					cfg.CodexForceWebsocket = force
					cfg.CodexResponseSteering = steering
					executor := &runtimeWebsocketCaptureExecutor{websocket: upstreamWS}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.RegisterExecutor(executor)
					auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
						Attributes: map[string]string{"websockets": "true"},
						Metadata:   map[string]any{"codex_cli": map[string]any{"enabled": true}}}
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					const model = "runtime-continuation-model"
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
					router := gin.New()
					router.GET("/v1/responses", h.ResponsesWebsocket)
					server := httptest.NewServer(router)
					defer server.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
					defer stop()
					for index, request := range []struct {
						body  string
						items int
					}{
						{`{"type":"response.create","model":"runtime-continuation-model","input":[{"type":"message","id":"user-1","role":"user","content":"first"}]}`, 1},
						{`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"user-2","role":"user","content":"second"}]}`, 3},
						{`{"type":"response.create","input":[{"type":"message","id":"root","role":"user","content":"new root"}]}`, 1},
						{`{"type":"response.append","input":[{"type":"message","id":"user-3","role":"user","content":"append"}]}`, 3},
						{`{"type":"response.create","input":[{"type":"compaction","encrypted_content":"compact-window"},{"type":"message","id":"compact-user","role":"user","content":"next window"}]}`, 2},
					} {
						if index == 0 && upstreamWS && force && !steering {
							// Runtime history is not subject to the direct force-bridge
							// replay cap; a large native window still needs its parent.
							request.body = strings.Replace(request.body, "first", strings.Repeat("x", 9<<20), 1)
						}
						if err := conn.WriteMessage(websocket.TextMessage, []byte(request.body)); err != nil {
							t.Fatal(err)
						}
						_, response, err := conn.ReadMessage()
						if err != nil || gjson.GetBytes(response, "type").String() != "response.completed" {
							t.Fatalf("round %d: response=%s err=%v", index+1, response, err)
						}
						payloads := executor.Payloads()
						if len(payloads) != index+1 {
							t.Fatalf("round %d: calls=%d", index+1, len(payloads))
						}
						body := payloads[index]
						if gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "input.#").Int() != int64(request.items) {
							t.Fatalf("round %d: history was not materialized: %s", index+1, body)
						}
					}
					executor.mu.Lock()
					defer executor.mu.Unlock()
					for _, required := range executor.requiredUpstreamWebsocket {
						if required {
							t.Fatal("CPA tried to steer a worker-owned socket")
						}
					}
				})
			}
		}
	}
}

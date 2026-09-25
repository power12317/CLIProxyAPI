package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Exercise the public Responses route, auth manager, real format translation,
// and upstream transport with native headers absent, as after new-api filtering.
func TestCodexRequestFidelityThroughResponsesRoutes(t *testing.T) {
	const model = "gpt-6-astra"
	const liteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"
	const include = `["reasoning.encrypted_content","web_search_call.action.sources"]`
	const metadataEvent = `{"type":"codex.response.metadata","headers":{"x-models-etag":"catalog-v1"},"future":{"enabled":false}}`
	const output = `[{"type":"reasoning","id":"reason-1","encrypted_content":"opaque","summary":[],"future":{"ok":true}},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}","future":7}]`
	const completed = `{"type":"response.completed","response":{"id":"r1","model":"gpt-6-astra","status":"completed","output":` + output + `,"future":{"flag":false},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`
	for _, transport := range []string{"json", "sse", "ws", "compact"} {
		for _, tc := range []struct {
			name, mirror, header, wantLite, tier string
			native                               bool
		}{
			{name: "filtered_true", mirror: "true", wantLite: "true", tier: "default", native: true},
			{name: "filtered_false", mirror: "false", wantLite: "false", tier: "ultrafast", native: true},
			{name: "filtered_absent", native: true},
			{name: "native_http_lite_body", wantLite: "true", tier: "default", native: true},
			{name: "explicit_header_false", mirror: "true", header: "false", wantLite: "false", tier: "priority", native: true},
			{name: "explicit_header_true", mirror: "false", header: "true", wantLite: "true", native: true},
			{name: "compat_default", tier: "default"},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				type capturedRequest struct {
					body    []byte
					headers http.Header
				}
				captured := make(chan capturedRequest, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if transport == "ws" {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						_, body, err := conn.ReadMessage()
						if err != nil {
							t.Error(err)
							return
						}
						captured <- capturedRequest{body, r.Header.Clone()}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(metadataEvent))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
						// Keep the reusable upstream alive until the downstream closes.
						_, _, _ = conn.ReadMessage()
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if r.Header.Get("Content-Encoding") == "zstd" {
						decoder, err := zstd.NewReader(nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer decoder.Close()
						body, err = decoder.DecodeAll(body, nil)
						if err != nil {
							t.Error(err)
							return
						}
					}
					captured <- capturedRequest{body, r.Header.Clone()}
					if transport == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"id":"r1","object":"response.compaction","output":` + output + `,"future":{"flag":false}}`))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", metadataEvent, completed)
				}))
				defer upstream.Close()
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				if transport == "ws" {
					manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
				} else {
					manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
				}
				auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL, "websockets": fmt.Sprint(transport == "ws"), coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}, Metadata: map[string]any{"access_token": "test-oauth", "account_id": "owner"}}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { helps.InvalidateCodexTurnStates(auth.ID); helps.InvalidateCodexCookieJar(auth.ID) })
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
				router := gin.New()
				router.POST("/v1/responses", h.Responses)
				router.POST("/v1/responses/compact", h.Compact)
				router.GET("/v1/responses", h.ResponsesWebsocket)
				downstream := httptest.NewServer(router)
				defer downstream.Close()
				body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}],"reasoning":{"effort":"high"},"parallel_tool_calls":false,"include":` + include + `}`)
				if tc.native {
					turn := map[string]any{"session_id": "session", "thread_id": "thread", "turn_id": "turn", "node_repl_auto_review_required": false, "tool_namespaces_info": map[string]any{"inventory": strings.Repeat("x", 20000)}}
					if tc.mirror != "" {
						turn["history_ingest_requested"] = tc.mirror == "true"
					}
					encoded, err := json.Marshal(turn)
					if err != nil {
						t.Fatal(err)
					}
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", string(encoded))
					body, _ = sjson.SetBytes(body, "client_metadata.x-openai-subagent", "collab_spawn")
					body, _ = sjson.SetBytes(body, "client_metadata.x-codex-parent-thread-id", "parent")
					if tc.name != "filtered_absent" {
						body, _ = sjson.SetBytes(body, "reasoning.context", "all_turns")
					}
					if tc.mirror != "" {
						body, _ = sjson.SetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite", tc.mirror == "true")
					}
				}
				if tc.tier != "" {
					body, _ = sjson.SetBytes(body, "service_tier", tc.tier)
				}
				headers := make(http.Header)
				if tc.header != "" {
					headers.Set(liteHeader, tc.header)
					headers.Set("X-OpenAI-Subagent", "explicit-subagent")
					headers.Set("X-Codex-Parent-Thread-Id", "explicit-parent")
					headers.Set("User-Agent", "Codex Desktop/0.156.1 (Mac OS; arm64) unknown")
					headers.Set("Originator", "Codex Desktop")
				}
				var response []byte
				if transport == "ws" {
					conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", headers)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					body, _ = sjson.SetBytes(body, "type", "response.create")
					if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
						t.Fatal(err)
					}
					for {
						_, frame, err := conn.ReadMessage()
						if err != nil {
							t.Fatal(err)
						}
						response = append(response, frame...)
						if typ := gjson.GetBytes(frame, "type").String(); typ == "response.completed" {
							break
						} else if typ == "error" || typ == "response.failed" {
							t.Fatalf("downstream error: %s", frame)
						}
					}
				} else {
					body, _ = sjson.SetBytes(body, "stream", transport == "sse")
					path := "/v1/responses"
					if transport == "compact" {
						path += "/compact"
					}
					req, err := http.NewRequest(http.MethodPost, downstream.URL+path, bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header = headers
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					response, err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || resp.StatusCode != 200 {
						t.Fatalf("response status=%d err=%v body=%s", resp.StatusCode, err, response)
					}
				}
				got := <-captured
				if tc.native {
					if gjson.GetBytes(got.body, "include").Raw != include {
						t.Errorf("include changed: %s", gjson.GetBytes(got.body, "include").Raw)
					}
					wantTier := tc.tier
					if wantTier == "default" {
						wantTier = ""
					}
					if gjson.GetBytes(got.body, "service_tier").String() != wantTier {
						t.Errorf("tier changed: %s", gjson.GetBytes(got.body, "service_tier").Raw)
					}
					if wantTier == "" && gjson.GetBytes(got.body, "service_tier").Exists() {
						t.Errorf("ordinary request sent service_tier: %s", got.body)
					}
					if gjson.GetBytes(got.body, "reasoning.effort").String() != "high" {
						t.Error("explicit reasoning changed")
					}
					bodyMetadata := gjson.Parse(gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String())
					headerMetadata := gjson.Parse(got.headers.Get("X-Codex-Turn-Metadata"))
					if !bodyMetadata.Get("tool_namespaces_info").Exists() || headerMetadata.Get("tool_namespaces_info").Exists() || len(got.headers.Get("X-Codex-Turn-Metadata")) > 3000 {
						t.Error("tool inventory must remain only in body")
					}
					for _, metadata := range []gjson.Result{bodyMetadata, headerMetadata} {
						if metadata.Get("node_repl_auto_review_required").Type != gjson.False {
							t.Error("B4 flag changed")
						}
						if flag := metadata.Get("history_ingest_requested"); flag.Exists() != (tc.mirror != "") || tc.mirror != "" && flag.Bool() != (tc.mirror == "true") {
							t.Errorf("history intent changed: %s", flag.Raw)
						}
					}
					wantParent, wantSubagent := "parent", "collab_spawn"
					if tc.header != "" {
						wantParent, wantSubagent = "explicit-parent", "explicit-subagent"
					}
					if got.headers.Get("X-Codex-Parent-Thread-Id") != wantParent || got.headers.Get("X-OpenAI-Subagent") != wantSubagent {
						t.Error("missing or overwritten relationship headers")
					}
					if !strings.Contains(got.headers.Get("X-Codex-Beta-Features"), "remote_compaction_v2") {
						t.Error("existing beta behavior removed")
					}
					if tc.header != "" && (got.headers.Get("User-Agent") != headers.Get("User-Agent") || got.headers.Get("Originator") != "Codex Desktop") {
						t.Error("desktop identity changed")
					}
					if transport == "sse" || transport == "ws" {
						if !bytes.Contains(response, []byte(metadataEvent)) {
							t.Errorf("native metadata response lost: %s", response)
						}
					}
				}
				wantLiteHeader := tc.wantLite
				if transport == "ws" && tc.native {
					// Body-only Lite intent belongs in each frame, not in the handshake.
					wantLiteHeader = tc.header
				}
				if got.headers.Get(liteHeader) != wantLiteHeader {
					t.Errorf("Lite header=%q want %q", got.headers.Get(liteHeader), wantLiteHeader)
				}
				if tc.native && (tc.mirror != "" || transport == "ws" && tc.wantLite != "") && gjson.GetBytes(got.body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String() != tc.wantLite {
					t.Error("resolved Lite header/body disagree")
				}
				for _, value := range []string{`"encrypted_content":"opaque"`, `"call_id":"call-1"`, `"future":{"flag":false}`} {
					if !bytes.Contains(response, []byte(value)) {
						t.Errorf("response field lost %s: %s", value, response)
					}
				}
			})
		}
	}
}

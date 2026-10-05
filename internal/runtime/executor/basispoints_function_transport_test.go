package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsFunctionRawTransportAcrossUpstreams(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
				for _, invalid := range []bool{false, true} {
					t.Run(fmt.Sprintf("ws=%t/stream=%t/%s/invalid=%t", ws, stream, field, invalid), func(t *testing.T) {
						var requests atomic.Int32
						const source = "  Write-Output \"中文\"\r\nC:\\workspace\\file"
						marker := "cpa.function_" + field + "/" + name
						metadata := "{\"workdir\":\"/tmp\",\"extra\":9007199254740993}"
						if invalid {
							metadata = "{invalid"
						}
						outer, _ := json.Marshal(map[string]any{"summary": marker, "code": source, "extended_summary": metadata, "references": []any{}, "destructive": false})
						completed, _ := json.Marshal(map[string]any{"type": "response.completed", "basispoints_replay_cursor": 0, "response": map[string]any{"id": "raw-result", "status": "completed", "output": []any{map[string]any{"type": "function_call", "name": "run_officejs", "id": "raw-item", "call_id": "raw-call", "arguments": string(outer)}}}})
						check := func(raw []byte) {
							requests.Add(1)
							if !strings.Contains(string(raw), marker) {
								t.Error("missing raw function catalog contract")
							}
						}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, raw, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							check(raw)
							_ = conn.WriteMessage(websocket.TextMessage, completed)
							_, _, _ = conn.ReadMessage()
						}))
						defer server.Close()
						cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: ws}}
						exec := NewCodexAutoExecutor(cfg)
						exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
						isolateBasispointsWebsockets(t, exec.basispointsExec)
						exec.basispointsExec.websocketDial = basispointsTestDial(server.URL)
						ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
							raw, _ := io.ReadAll(r.Body)
							check(raw)
							if ws || r.URL.Host != "bps.openai.com" {
								t.Error("wrong selected upstream")
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(completed) + "\n\n")), Request: r}, nil
						})))
						body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "inspect", "tools": []any{map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{field: map[string]any{"type": "string"}}}}}})
						req, opts := core.Request{Model: "gpt-6-astra", Payload: body}, core.Options{SourceFormat: translator.FormatOpenAIResponse}
						var final []byte
						var err error
						if stream {
							var result *core.StreamResult
							result, err = exec.ExecuteStream(ctx, basispointsTestAuth(), req, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
									if done := basispoints.CompletedResponse(chunk.Payload); len(done) > 0 {
										final = done
									}
								}
							}
						} else {
							var result core.Response
							result, err = exec.Execute(ctx, basispointsTestAuth(), req, opts)
							final = result.Payload
						}
						if requests.Load() != 1 {
							t.Fatal("tool response triggered an extra upstream request")
						}
						if invalid {
							if err == nil || !strings.Contains(err.Error(), "invalid_tool_envelope") {
								t.Fatalf("invalid format was not reported: %v", err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						call := gjson.GetBytes(final, "output.0")
						args := gjson.Parse(call.Get("arguments").String())
						if call.Get("name").String() != name || args.Get(field).String() != source || args.Get("workdir").String() != "/tmp" || args.Get("extra").Raw != "9007199254740993" || call.Get("encrypted_function_args").Raw != "[]" {
							t.Fatalf("raw arguments changed: %s", final)
						}
					})
				}
			}
		}
	}
}

package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBasispointsUnmappedCapabilitiesSwitchBeforeClientPublication(t *testing.T) {
	for _, mode := range []string{"http", "forced_basispoints_ws", "downstream_ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				const source = "return 'exact source';\n"
				const declaration = `{"type":"function","name":"run_code","parameters":{"type":"object","required":["code"],"properties":{"code":{"type":"string"}}}}`
				var bpsCalls, nativeCalls atomic.Int32
				tools := []any{}
				if err := json.Unmarshal([]byte("["+declaration+"]"), &tools); err != nil {
					t.Fatal(err)
				}
				nativeResponse := `{"id":"native-response","status":"completed","output":[{"type":"function_call","id":"client-item","call_id":"client-call","name":"run_code","arguments":"{\"code\":\"return 'exact source';\\n\"}"}]}`
				checkNative := func(body []byte) {
					if ojson.GetBytes(body, "tools.0.name").String() != "run_code" || ojson.GetBytes(body, "input.0.content").String() != "original question" {
						t.Error("channel switch changed the original client request")
					}
				}
				nativeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					nativeCalls.Add(1)
					checkNative(raw)
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":`+nativeResponse+`}`))
					_, _, _ = conn.ReadMessage()
				}))
				defer nativeServer.Close()
				bpsEvents := func(body []byte) string {
					call := bpsCalls.Add(1)
					if call > 1 {
						if !strings.Contains(string(body), "actual client result") {
							t.Error("next turn lost the real client tool result")
						}
						return "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"continued\",\"status\":\"completed\",\"output\":[]}}\n\n"
					}
					outer, _ := json.Marshal(map[string]any{"summary": "cpa.function_code/run_code", "code": source, "extended_summary": "Run the declared client code."})
					batch := []any{
						map[string]any{"type": "function_call", "name": "run_officejs", "id": "candidate-tool", "call_id": "candidate-call", "arguments": string(outer)},
						map[string]any{"type": "function_call", "name": "list_skills", "id": "provider-tool", "call_id": "provider-call", "arguments": `{"limit":8}`},
					}
					event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "discarded-response", "status": "completed", "output": batch}})
					return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"discarded-response\",\"output\":[]}}\n\n" +
						"data: {\"type\":\"response.output_text.delta\",\"delta\":\"speculative provider text\"}\n\n" +
						"data: " + string(event) + "\n\n"
				}
				bpsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						_, raw, err := conn.ReadMessage()
						if err != nil {
							return
						}
						for _, line := range strings.Split(bpsEvents(raw), "\n") {
							if strings.HasPrefix(line, "data: ") {
								if err := conn.WriteMessage(websocket.TextMessage, []byte(line[6:])); err != nil {
									return
								}
							}
						}
					}
				}))
				defer bpsServer.Close()
				cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: mode == "forced_basispoints_ws"}}
				exec := NewCodexAutoExecutor(cfg)
				exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
				isolateBasispointsWebsockets(t, exec.basispointsExec)
				exec.basispointsExec.websocketDial = basispointsTestDial(bpsServer.URL)
				auth := basispointsTestAuth()
				auth.Attributes = map[string]string{"base_url": nativeServer.URL, "websockets": "true"}
				session := t.Name()
				defer exec.CloseExecutionSession(session)
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(r.Body)
					events := ""
					if r.URL.Host == "bps.openai.com" {
						events = bpsEvents(body)
					} else {
						nativeCalls.Add(1)
						checkNative(body)
						events = "data: {\"type\":\"response.completed\",\"response\":" + nativeResponse + "}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: r}, nil
				})))
				if mode == "downstream_ws" {
					ctx = core.WithDownstreamWebsocket(ctx)
				}
				opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Metadata: map[string]any{core.ExecutionSessionMetadataKey: session}}
				invoke := func(input []any) []byte {
					t.Helper()
					body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": input, "tools": tools})
					req := core.Request{Model: "gpt-6-astra", Payload: body}
					if !stream {
						response, err := exec.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						return response.Payload
					}
					response, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					var completed []byte
					var observed strings.Builder
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						observed.Write(chunk.Payload)
						if strings.Contains(string(chunk.Payload), "discarded-response") || strings.Contains(string(chunk.Payload), "speculative provider text") || strings.Contains(string(chunk.Payload), "candidate-call") {
							t.Fatal("candidate content or a tool side effect escaped before channel selection")
						}
						if payload := basispoints.CompletedResponse(chunk.Payload); len(payload) > 0 {
							completed = payload
						}
						if ojson.GetBytes(chunk.Payload, "type").String() == "response.completed" {
							completed = []byte(ojson.GetBytes(chunk.Payload, "response").Raw)
						}
					}
					if len(completed) == 0 {
						t.Fatalf("stream never completed: %s", observed.String())
					}
					return completed
				}
				input := []any{map[string]any{"role": "user", "content": "original question"}}
				first := invoke(input)
				var actualCall map[string]any
				if err := json.Unmarshal([]byte(ojson.GetBytes(first, "output.0").Raw), &actualCall); err != nil {
					t.Fatal(err)
				}
				if ojson.GetBytes([]byte(actualCall["arguments"].(string)), "code").String() != source {
					t.Fatal("final mapped client source changed")
				}
				input = append(input, actualCall, map[string]any{"type": "function_call_output", "call_id": "client-call", "output": "actual client result"})
				if next := invoke(input); ojson.GetBytes(next, "id").String() != "continued" || bpsCalls.Load() != 2 || nativeCalls.Load() != 1 {
					t.Fatalf("channel routing or next-turn replay repeated execution: bps=%d native=%d", bpsCalls.Load(), nativeCalls.Load())
				}
			})
		}
	}
}

func TestBasispointsCapabilityDecisionCancellationStopsBeforeFallback(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
	exec := NewCodexAutoExecutor(cfg)
	exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	entered := make(chan struct{})
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var nativeCalls atomic.Int32
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "bps.openai.com" {
			nativeCalls.Add(1)
			return nil, fmt.Errorf("unexpected fallback")
		}
		go func() {
			<-req.Context().Done()
			_ = reader.CloseWithError(req.Context().Err())
			close(stopped)
		}()
		close(entered)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: req}, nil
	})))
	done := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteStream(ctx, basispointsTestAuth(), core.Request{
			Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"hello","tools":[{"type":"function","name":"local"}]}`),
		}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancellation returned %v", err)
	}
	<-stopped
	if nativeCalls.Load() != 0 {
		t.Fatal("canceled capability decision generated another model request")
	}
}

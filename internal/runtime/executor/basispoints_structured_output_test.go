package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBasispointsStructuredOutputAcrossTransportsAndResponseModes(t *testing.T) {
	for _, transport := range []string{"sse", "json", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, valid := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/valid=%v", transport, stream, valid), func(t *testing.T) {
					cfg := forceWebsocketTestConfig()
					cfg.RequestRetry = 3
					cfg.Codex.ForceWebsocket = transport == "ws"
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "codex"}}, Params: map[string]any{"input.0.content.0.text": "configured instructions"}}}}
					exec := NewBasispointsExecutor(cfg)
					isolateBasispointsWebsockets(t, exec)
					var calls atomic.Int32
					check := func(raw []byte) {
						calls.Add(1)
						if ojson.GetBytes(raw, "input.0.content.0.text").String() != "configured instructions" || ojson.GetBytes(raw, "text").Exists() {
							t.Errorf("structured preparation changed final payload rules: %s", raw)
						}
					}
					answer := `{"ok":true}`
					if !valid {
						answer = "private invalid final answer"
					}
					response := map[string]any{"id": "structured", "model": "test-model", "status": "completed", "output": []any{map[string]any{"type": "message", "id": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}}
					terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					const early = `{"type":"response.output_text.delta","item_id":"message","delta":"private early text"}`
					ctx := t.Context()
					if transport == "ws" {
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
							_ = conn.WriteMessage(websocket.TextMessage, []byte(early))
							_ = conn.WriteMessage(websocket.TextMessage, terminal)
							_, _, _ = conn.ReadMessage()
						}))
						defer server.Close()
						exec.websocketDial = basispointsTestDial(server.URL)
					} else {
						ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
							raw, err := io.ReadAll(r.Body)
							if err != nil {
								return nil, err
							}
							check(raw)
							contentType, body := "text/event-stream", "data: "+early+"\n\ndata: "+string(terminal)+"\n\n"
							if transport == "json" {
								contentType = "application/json"
								raw, _ := json.Marshal(response)
								body = string(raw)
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
						})))
					}
					req := core.Request{Model: "test-model", Payload: []byte(`{"input":"answer","text":{"format":{"type":"json_object"}}}`)}
					var output bytes.Buffer
					var resultErr error
					if stream {
						result, err := exec.ExecuteStream(ctx, basispointsTestAuth(), req, core.Options{})
						resultErr = err
						if err == nil {
							for chunk := range result.Chunks {
								output.Write(chunk.Payload)
								if chunk.Err != nil {
									resultErr = chunk.Err
								}
							}
						}
					} else {
						result, err := exec.Execute(ctx, basispointsTestAuth(), req, core.Options{})
						resultErr = err
						output.Write(result.Payload)
					}
					if (resultErr == nil) != valid {
						t.Fatalf("unexpected output %s error %v", output.String(), resultErr)
					}
					if strings.Contains(output.String(), "private") {
						t.Fatal("unvalidated text reached client")
					}
					if valid && !strings.Contains(output.String(), `\"ok\":true`) {
						t.Fatalf("validated answer lost: %s", output.String())
					}
					if calls.Load() != 1 {
						t.Fatalf("generation regenerated %d times", calls.Load())
					}
				})
			}
		}
	}
}

func TestBasispointsChatCompletionOutputFormatIsRetained(t *testing.T) {
	for _, format := range []string{
		`{"type":"json_object"}`,
		`{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","required":["ok"]}}}`,
	} {
		for _, valid := range []bool{false, true} {
			cfg := forceWebsocketTestConfig()
			cfg.Codex.ForceWebsocket = false
			exec := NewBasispointsExecutor(cfg)
			var calls atomic.Int32
			answer := "not JSON"
			if valid {
				answer = `{"ok":true}`
			}
			raw, _ := json.Marshal(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "id": "answer", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "gateway validates") {
					t.Error("Chat Completions format lost before BPS preparation")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
			})))
			payload := []byte(`{"messages":[{"role":"user","content":"answer"}],"response_format":` + format + `}`)
			_, err := exec.Execute(ctx, basispointsTestAuth(), core.Request{Model: "test-model", Payload: payload}, core.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAIResponse})
			if (err == nil) != valid || calls.Load() != 1 {
				t.Fatalf("format=%s valid=%v calls=%d err=%v", format, valid, calls.Load(), err)
			}
		}
	}
}

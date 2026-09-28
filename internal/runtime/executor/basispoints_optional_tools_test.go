package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestBasispointsOptionalToolsRouteOnInvocation(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"local", "discovery", "native", "malformed"} {
				t.Run(fmt.Sprintf("ws=%t/stream=%t/%s", ws, stream, mode), func(t *testing.T) {
					var bpsCalls, nativeCalls, bpsDials atomic.Int32
					tools := []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}}, map[string]any{"type": "tool_search", "execution": "client", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}, map[string]any{"type": "web_search", "external_web_access": true}}
					input := []any{map[string]any{"role": "user", "content": "Translate the schedule in C:/fixture/book.xlsx using local tools."}}
					source, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "tool_choice": "auto", "input": input, "tools": tools})
					before := bytes.Clone(source)
					marshal := func(value any) []byte {
						raw, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						return raw
					}
					localOuter := marshal(map[string]any{"summary": "cpa.function_cmd/exec_command", "code": "Get-Item C:/fixture/book.xlsx", "extended_summary": "{}"})
					localCall := map[string]any{"type": "function_call", "name": "run_officejs", "id": "local-item", "call_id": "local-call", "arguments": string(localOuter)}
					nativeDone := marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "native-result", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Native capability completed"}}}}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}})
					nativeResponse := func(raw []byte) []byte {
						nativeCalls.Add(1)
						var got map[string]any
						if json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(got["tools"], tools) || !reflect.DeepEqual(got["input"], input) || got["tool_choice"] != "auto" {
							t.Error("native switch altered the original tools, history or tool choice")
						}
						return nativeDone
					}
					bpsResponse := func(raw []byte) []byte {
						count := bpsCalls.Add(1)
						if !bytes.Contains(raw, []byte("cpa.native/web_search")) || !bytes.Contains(raw, []byte("tool_search")) {
							t.Error("capabilities removed from Basispoints catalog")
						}
						output := []any{localCall}
						if count == 1 {
							switch mode {
							case "discovery":
								envelope := marshal(map[string]any{"name": "tool_search", "arguments": map[string]any{"query": "spreadsheet editor"}})
								outer := marshal(map[string]any{"code": string(envelope)})
								output = []any{map[string]any{"type": "function_call", "name": "run_officejs", "id": "search-item", "call_id": "search-call", "arguments": string(outer)}}
							case "native", "malformed":
								outer := marshal(map[string]any{"summary": "cpa.native/web_search", "code": "", "extended_summary": "Need web results"})
								if mode == "malformed" {
									outer = marshal(map[string]any{"code": "not a JSON envelope"})
								}
								output = append(output, map[string]any{"type": "function_call", "name": "run_officejs", "id": "native-intent", "call_id": "native-intent", "arguments": string(outer)})
							}
						}
						return marshal(map[string]any{"type": "response.completed", "basispoints_replay_cursor": 1, "response": map[string]any{"id": "bps-result", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1, "total_tokens": 6}}})
					}
					delta := marshal(map[string]any{"type": "response.output_text.delta", "basispoints_replay_cursor": 0, "delta": "SPECULATIVE_BPS_TEXT"})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						bps := strings.Contains(r.URL.Path, "basispoints")
						if bps {
							bpsDials.Add(1)
						}
						for {
							_, raw, errRead := conn.ReadMessage()
							if errRead != nil {
								return
							}
							var done []byte
							if bps {
								done = bpsResponse(raw)
								_ = conn.WriteMessage(websocket.TextMessage, delta)
							} else {
								done = nativeResponse(raw)
							}
							if errWrite := conn.WriteMessage(websocket.TextMessage, done); errWrite != nil {
								return
							}
						}
					}))
					defer server.Close()
					cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}, Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
					exec := NewCodexAutoExecutor(cfg)
					exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
					isolateBasispointsWebsockets(t, exec.basispointsExec)
					exec.basispointsExec.websocketDial = basispointsTestDial(server.URL)
					auth := basispointsTestAuth()
					auth.ID = t.Name()
					auth.Attributes = map[string]string{"base_url": server.URL, "websockets": "true"}
					t.Cleanup(func() { exec.CloseExecutionSession(t.Name()) })
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						raw, _ := io.ReadAll(r.Body)
						var events string
						if r.URL.Host == "bps.openai.com" {
							events = "data: " + string(delta) + "\n\ndata: " + string(bpsResponse(raw)) + "\n\n"
						} else {
							events = "data: " + string(nativeResponse(raw)) + "\n\n"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: r}, nil
					})))
					if ws {
						ctx = core.WithDownstreamWebsocket(ctx)
					}
					req := core.Request{Model: "gpt-6-astra", Payload: source}
					opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name(), core.CallerScopeMetadataKey: t.Name(), core.CanonicalSessionIDMetadataKey: t.Name()}}
					run := func() ([]byte, error) {
						if !stream {
							result, err := exec.Execute(ctx, auth, req, opts)
							return result.Payload, err
						}
						result, err := exec.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							return nil, err
						}
						var out bytes.Buffer
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								return out.Bytes(), chunk.Err
							}
							out.Write(chunk.Payload)
						}
						return out.Bytes(), nil
					}
					out, err := run()
					if !bytes.Equal(source, before) {
						t.Fatal("original request bytes changed")
					}
					if mode == "malformed" {
						if err == nil || nativeCalls.Load() != 0 || bpsCalls.Load() != 1 {
							t.Fatalf("format failure was retried: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if bpsCalls.Load() != 1 {
						t.Fatal("unexpected Basispoints retry")
					}
					if mode == "native" {
						if nativeCalls.Load() != 1 || !bytes.Contains(out, []byte("native-result")) || bytes.Contains(out, []byte("SPECULATIVE_BPS_TEXT")) || bytes.Contains(out, []byte("local-call")) {
							t.Fatalf("native switch leaked speculative output: %s", out)
						}
						if _, err := run(); err != nil {
							t.Fatal(err)
						}
						if nativeCalls.Load() != 1 || bpsCalls.Load() != 2 || ws && bpsDials.Load() != 1 {
							t.Fatal("native switch closed the reusable Basispoints socket")
						}
					} else {
						if nativeCalls.Load() != 0 || !bytes.Contains(out, []byte("bps-result")) {
							t.Fatal("optional capability caused native routing")
						}
						if mode == "discovery" && !bytes.Contains(out, []byte("tool_search_call")) {
							t.Fatal("client discovery not returned")
						}
					}
				})
			}
		}
	}
}

func TestBasispointsOptionalToolsCancellationDoesNotSwitch(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	written := make(chan struct{})
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pending decision\"}\n\n")
		close(written)
	}()
	var nativeCalls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "bps.openai.com" {
			nativeCalls.Add(1)
			return nil, errors.New("unexpected native call")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}, nil
	})))
	exec := NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}})
	exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "local task", "tools": []any{map[string]any{"type": "web_search"}}})
	done := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteStream(ctx, basispointsTestAuth(), core.Request{Model: "gpt-6-astra", Payload: body}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
		done <- err
	}()
	select {
	case <-written:
	case err := <-done:
		t.Fatalf("response published before tool decision: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation changed: %v", err)
	}
	if nativeCalls.Load() != 0 {
		t.Fatal("canceled decision started native generation")
	}
}

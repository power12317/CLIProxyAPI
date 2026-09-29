package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsWebsocketRetryWaitKeepsConnection(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelWait), func(t *testing.T) {
			var connections, calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				connections.Add(1)
				defer func() { _ = conn.Close() }()
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
					if calls.Add(1) == 1 {
						_ = conn.WriteJSON(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "rate_limit_exceeded", "headers": map[string]any{"retry-after-ms": "60000"}}, "output": []any{}}})
					} else {
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "reused-after-wait", "status": "completed", "output": []any{}}})
					}
				}
			}))
			t.Cleanup(server.Close)
			cfg := &config.Config{RequestRetry: 3, MaxRetryInterval: 1, Codex: config.CodexConfig{ForceWebsocket: true}}
			if cancelWait {
				cfg.MaxRetryInterval = 120
			}
			exec := NewBasispointsExecutor(cfg)
			isolateBasispointsWebsockets(t, exec)
			exec.websocketDial = basispointsTestDial(server.URL)
			auth := basispointsTestAuth()
			auth.ID = t.Name()
			payload, _ := json.Marshal(map[string]any{"input": "continue", "client_metadata": map[string]any{"session_id": "wait-session", "turn_id": "wait-turn"}})
			req := coreexecutor.Request{Model: "gpt-6-astra", Payload: payload}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reporter := helps.NewExecutorUsageReporter(ctx, exec, req.Model, auth)
			response, bridge, _, err := exec.open(ctx, auth, req, opts, reporter)
			if err != nil {
				t.Fatal(err)
			}
			observed := make(chan struct{})
			previous := bridge.ObserveEvent
			bridge.ObserveEvent = func(raw []byte) {
				previous(raw)
				if gjson.GetBytes(raw, "type").String() == "response.failed" {
					close(observed)
				}
			}
			done := make(chan error, 1)
			go func() {
				errRead := exec.read(ctx, auth, response, bridge, true, false, func([]byte) error { t.Error("failed attempt emitted output"); return nil })
				_ = response.Body.Close()
				done <- errRead
			}()
			<-observed
			if cancelWait {
				cancel()
			}
			err = <-done
			if cancelWait {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("wait ignored cancellation: %v", err)
				}
			} else {
				var upstream *basispoints.Error
				if !errors.As(err, &upstream) || upstream.StatusCode() != 429 {
					t.Fatalf("wait limit lost original failure: %v", err)
				}
			}
			if calls.Load() != 1 || connections.Load() != 1 {
				t.Fatalf("unexpected retry during wait: calls=%d connections=%d", calls.Load(), connections.Load())
			}
			if _, errNext := exec.Execute(t.Context(), auth, req, opts); errNext != nil || calls.Load() != 2 || connections.Load() != 1 {
				t.Fatalf("wait closed the healthy websocket: %v calls=%d connections=%d", errNext, calls.Load(), connections.Load())
			}
		})
	}
}

func TestBasispointsWebsocketRetryMatchesSSE(t *testing.T) {
	for _, status := range []int{400, 403, 404, 408, 422, 429, 500, 502, 503, 504, 505} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/stop=%t", status, stop), func(t *testing.T) {
				counts := map[string]int32{}
				for _, transport := range []string{"sse", "websocket"} {
					cfg := &config.Config{RequestRetry: 3, MaxRetryInterval: 1, Codex: config.CodexConfig{ForceWebsocket: transport == "websocket", Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
					exec := NewCodexAutoExecutor(cfg)
					exec.basispointsExec.cooldown = basispoints.NewCooldown(time.Now)
					ctx, calls := basispointsFailureTransport(t, exec.basispointsExec, transport, false, "0", status)
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(3, time.Second, 1)
					manager.RegisterExecutor(exec)
					auth := basispointsTestAuth()
					auth.ID = t.Name() + "/" + transport
					auth.Metadata["disable_cooling"] = true
					if stop {
						auth.Metadata["request_scoped_errors"] = []config.RequestScopedErrorRule{{Status: status, Match: []string{"upstream_failure"}, Action: "stop"}}
					}
					if _, err := manager.Register(ctx, auth); err != nil {
						t.Fatal(err)
					}
					const model = "basispoints-retry-parity"
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					payload, _ := json.Marshal(map[string]any{"model": model, "input": "continue"})
					result, err := manager.ExecuteStream(ctx, []string{"codex"}, coreexecutor.Request{Model: model, Payload: payload}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
					if err == nil {
						for chunk := range result.Chunks {
							if len(chunk.Payload) > 0 {
								t.Errorf("failed request emitted payload: %s", chunk.Payload)
							}
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
					if err == nil {
						t.Fatal("upstream failure lost")
					}
					counts[transport] = calls.Load()
				}
				if counts["sse"] != counts["websocket"] {
					t.Fatalf("request-retry policy differs: %v", counts)
				}
				if stop && counts["websocket"] != 1 {
					t.Fatalf("stop rule ignored: %v", counts)
				}
			})
		}
	}
}

func TestBasispointsBufferedRetryMatchesSSE(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/optional_stream=%t", transport, stream), func(t *testing.T) {
				cfg := &config.Config{RequestRetry: 3, MaxRetryInterval: 1, Codex: config.CodexConfig{ForceWebsocket: transport == "websocket", Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
				exec := NewCodexAutoExecutor(cfg)
				exec.basispointsExec.cooldown = basispoints.NewCooldown(time.Now)
				ctx, calls := basispointsFailureTransport(t, exec.basispointsExec, transport, true, "0")
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.SetRetryConfig(3, time.Second, 1)
				manager.RegisterExecutor(exec)
				auth := basispointsTestAuth()
				auth.ID = t.Name()
				auth.Metadata["disable_cooling"] = true
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				const model = "basispoints-buffered-retry"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				body := map[string]any{"model": model, "input": "continue"}
				if stream {
					body["tools"] = []any{map[string]any{"type": "web_search"}}
				}
				payload, _ := json.Marshal(body)
				req := coreexecutor.Request{Model: model, Payload: payload}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var err error
				if stream {
					var result *coreexecutor.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if len(chunk.Payload) > 0 {
								t.Error("failed candidate escaped decision buffer")
							}
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					var result coreexecutor.Response
					result, err = manager.Execute(ctx, []string{"codex"}, req, opts)
					if len(result.Payload) > 0 {
						t.Error("failed non-stream request returned output")
					}
				}
				assertBasispointsRateLimit(t, err, 0)
				if calls.Load() != 4 {
					t.Fatalf("unpublished output stopped configured retries: %d", calls.Load())
				}
			})
		}
	}
}

func TestBasispointsWebsocketRetryDropsBufferedCandidate(t *testing.T) {
	var calls, connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections.Add(1)
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
			if calls.Add(1) == 1 {
				_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "basispoints_replay_cursor": 0, "delta": "rejected candidate"})
				_ = conn.WriteJSON(map[string]any{"type": "response.failed", "basispoints_replay_cursor": 1, "response": map[string]any{"status": "failed", "error": map[string]any{"status_code": 500, "message": "retry upstream error"}, "output": []any{}}})
			} else {
				_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "basispoints_replay_cursor": 0, "delta": "accepted candidate"})
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "basispoints_replay_cursor": 1, "response": map[string]any{"id": "winner", "status": "completed", "output": []any{}}})
			}
		}
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{RequestRetry: 3, MaxRetryInterval: 1, Codex: config.CodexConfig{ForceWebsocket: true, Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
	exec := NewCodexAutoExecutor(cfg)
	exec.basispointsExec.cooldown = basispoints.NewCooldown(time.Now)
	isolateBasispointsWebsockets(t, exec.basispointsExec)
	exec.basispointsExec.websocketDial = basispointsTestDial(server.URL)
	payload, _ := json.Marshal(map[string]any{"input": "continue", "tools": []any{map[string]any{"type": "web_search"}}})
	result, err := exec.ExecuteStream(t.Context(), basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: payload}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if calls.Load() != 2 || connections.Load() != 1 || !strings.Contains(output.String(), "accepted candidate") || strings.Contains(output.String(), "rejected candidate") {
		t.Fatalf("wrong retry publication: calls=%d connections=%d output=%s", calls.Load(), connections.Load(), output.String())
	}
}

func TestBasispointsWebsocketRetryKeepsConnectionAndRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name                                    string
			status, failures, limit, override, want int
			success                                 bool
		}{
			{"rate limit recovery", 429, 2, 3, -1, 3, true},
			{"request timeout recovery", 408, 1, 3, -1, 2, true},
			{"server error recovery", 500, 1, 3, -1, 2, true},
			{"unavailable recovery", 503, 1, 3, -1, 2, true},
			{"gateway timeout recovery", 504, 1, 3, -1, 2, true},
			{"exhausted", 503, 10, 3, -1, 4, false},
			{"disabled", 429, 10, 0, -1, 1, false},
			{"auth override", 429, 10, 3, 1, 2, false},
			{"auth disables", 429, 10, 3, 0, 1, false},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				var connections, calls atomic.Int32
				frames := make(chan []byte, 16)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					connections.Add(1)
					defer func() { _ = conn.Close() }()
					for {
						_, frame, errRead := conn.ReadMessage()
						if errRead != nil {
							return
						}
						frames <- frame
						attempt := calls.Add(1)
						id := fmt.Sprintf("attempt-%d", attempt)
						_ = conn.WriteJSON(map[string]any{"type": "response.created", "basispoints_replay_cursor": 0, "response": map[string]any{"id": id}})
						if int(attempt) <= tc.failures {
							item := map[string]any{"type": "function_call", "id": fmt.Sprintf("partial-%d", attempt), "call_id": "partial", "name": "run_officejs", "status": "in_progress", "arguments": "{"}
							_ = conn.WriteJSON(map[string]any{"type": "response.output_item.added", "basispoints_replay_cursor": 1, "item": item})
							_ = conn.WriteJSON(map[string]any{"type": "response.failed", "basispoints_replay_cursor": 2, "response": map[string]any{"id": id, "status": "failed", "error": map[string]any{"code": "upstream_failure", "status_code": tc.status, "message": "retry this rejected attempt", "headers": map[string]any{"retry-after-ms": "0"}}, "output": []any{item}}})
						} else {
							_ = conn.WriteJSON(map[string]any{"type": "response.completed", "basispoints_replay_cursor": 1, "response": map[string]any{"id": id, "status": "completed", "output": []any{}}})
						}
					}
				}))
				t.Cleanup(server.Close)
				cfg := &config.Config{RequestRetry: tc.limit, MaxRetryInterval: 1, Codex: config.CodexConfig{ForceWebsocket: true}}
				cfg.RequestLog = true
				exec := NewBasispointsExecutor(cfg)
				isolateBasispointsWebsockets(t, exec)
				exec.websocketDial = basispointsTestDial(server.URL)
				auth := basispointsTestAuth()
				auth.ID = t.Name()
				if tc.override >= 0 {
					auth.Metadata["request_retry"] = tc.override
				}
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				ctx := context.WithValue(t.Context(), "gin", ginCtx)
				payload, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "preserve this request", "client_metadata": map[string]any{"session_id": "retry-session", "turn_id": "retry-turn"}})
				req := coreexecutor.Request{Model: "gpt-6-astra", Payload: payload}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var output bytes.Buffer
				var err error
				if stream {
					var result *coreexecutor.StreamResult
					result, err = exec.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							output.Write(chunk.Payload)
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					var result coreexecutor.Response
					result, err = exec.Execute(ctx, auth, req, opts)
					output.Write(result.Payload)
				}
				if (err == nil) != tc.success || calls.Load() != int32(tc.want) || connections.Load() != 1 {
					t.Fatalf("success=%t calls=%d connections=%d error=%v", err == nil, calls.Load(), connections.Load(), err)
				}
				if !tc.success {
					var upstream *basispoints.Error
					if !errors.As(err, &upstream) || upstream.StatusCode() != tc.status {
						t.Fatalf("final upstream error lost: %v", err)
					}
				}
				if strings.Contains(output.String(), "partial-") || strings.Contains(output.String(), "response.failed") {
					t.Fatalf("failed attempt leaked downstream: %s", output.String())
				}
				if tc.success && strings.Contains(output.String(), "attempt-1") {
					t.Fatal("failed preamble leaked downstream")
				}
				seen := map[string]bool{}
				var original map[string]any
				logText := string(ginCtx.MustGet("API_REQUEST").([]byte))
				for i := 0; i < tc.want; i++ {
					frame := <-frames
					var value map[string]any
					if errDecode := json.Unmarshal(frame, &value); errDecode != nil {
						t.Fatal(errDecode)
					}
					for _, field := range []string{"basispoints_request_id", "basispoints_resume_token"} {
						id, _ := value[field].(string)
						if id == "" || seen[id] {
							t.Fatalf("reused failed generation identity: %s", field)
						}
						seen[id] = true
						if field == "basispoints_request_id" && !strings.Contains(logText, id) {
							t.Fatal("retry request missing from log")
						}
						if field == "basispoints_resume_token" && strings.Contains(logText, id) {
							t.Fatal("retry log leaked resume capability")
						}
						delete(value, field)
					}
					if i == 0 {
						original = value
					} else if !reflect.DeepEqual(original, value) {
						t.Fatal("retry changed request content, session, or turn")
					}
					if value["type"] != "response.create" {
						t.Fatal("retry resumed the failed generation instead of resending")
					}
				}
				if tc.success {
					if _, errNext := exec.Execute(ctx, auth, req, opts); errNext != nil || connections.Load() != 1 {
						t.Fatalf("successful retry lost session reuse: %v connections=%d", errNext, connections.Load())
					}
				}
			})
		}
	}
}

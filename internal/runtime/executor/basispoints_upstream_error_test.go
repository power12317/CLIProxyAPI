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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func basispointsFailureTransport(t *testing.T, exec *BasispointsExecutor, transport string, priorText bool, retryMS string, failureStatus ...int) (context.Context, *atomic.Int32) {
	t.Helper()
	marshal := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	item := map[string]any{"type": "function_call", "id": "partial-item", "call_id": "partial-call", "name": "run_officejs", "status": "in_progress", "arguments": "{"}
	response := map[string]any{"id": "failed-response", "status": "failed", "error": map[string]any{"code": "rate_limit_exceeded", "message": "Rate limit reached on tokens per min (TPM).", "headers": map[string]any{"retry-after": "1", "retry-after-ms": retryMS}}, "output": []any{item}}
	if len(failureStatus) > 0 {
		detail := response["error"].(map[string]any)
		detail["status_code"], detail["code"], detail["message"] = failureStatus[0], "upstream_failure", "upstream rejected this attempt"
	}
	terminal := marshal(map[string]any{"type": "response.failed", "response": response, "basispoints_replay_cursor": 2})
	frames := [][]byte{}
	if priorText {
		frames = append(frames, marshal(map[string]any{"type": "response.output_text.delta", "delta": "already streamed", "basispoints_replay_cursor": 0}))
	}
	frames = append(frames, marshal(map[string]any{"type": "response.output_item.added", "item": item, "basispoints_replay_cursor": 1}), terminal)
	calls := &atomic.Int32{}
	ctx := t.Context()
	if transport == "websocket" {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			for {
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				calls.Add(1)
				for _, frame := range frames {
					if err = conn.WriteMessage(websocket.TextMessage, frame); err != nil {
						return
					}
				}
			}
		}))
		t.Cleanup(server.Close)
		isolateBasispointsWebsockets(t, exec)
		exec.websocketDial = basispointsTestDial(server.URL)
	} else {
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.URL.String() != basispoints.ResponsesURL {
				t.Errorf("unexpected route: %s", r.URL)
			}
			contentType, body := "application/json", marshal(response)
			if transport == "sse" {
				contentType = "text/event-stream"
				var wire bytes.Buffer
				for _, frame := range frames {
					fmt.Fprintf(&wire, "data: %s\n\n", frame)
				}
				body = wire.Bytes()
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
		})))
	}
	return ctx, calls
}

func assertBasispointsRateLimit(t *testing.T, err error, delay time.Duration) {
	t.Helper()
	var upstream *basispoints.Error
	if !errors.As(err, &upstream) || upstream.StatusCode() != 429 {
		t.Fatalf("expected upstream 429, got %v", err)
	}
	code := gjson.Get(upstream.Body, "response.error.code").String()
	if code == "" {
		code = gjson.Get(upstream.Body, "error.code").String()
	}
	if code != "rate_limit_exceeded" || !strings.Contains(upstream.Body, "tokens per min (TPM)") || strings.Contains(upstream.Body, "incomplete_tools") {
		t.Fatalf("upstream error replaced: %s", upstream.Body)
	}
	if upstream.RetryAfter() == nil || *upstream.RetryAfter() != delay {
		t.Fatalf("retry hint = %v, want %v", upstream.RetryAfter(), delay)
	}
}

func TestBasispointsUpstreamFailureAcrossTransports(t *testing.T) {
	for _, transport := range []string{"json", "sse", "websocket"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", transport, stream), func(t *testing.T) {
				cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: transport == "websocket"}}
				exec := NewBasispointsExecutor(cfg)
				ctx, calls := basispointsFailureTransport(t, exec, transport, false, "15")
				payload, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "continue", "tools": []any{map[string]any{"type": "function", "name": "shell"}}})
				req := coreexecutor.Request{Model: "gpt-6-astra", Payload: payload}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var err error
				if stream {
					var result *coreexecutor.StreamResult
					result, err = exec.ExecuteStream(ctx, basispointsTestAuth(), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if len(chunk.Payload) != 0 {
							t.Errorf("failed response emitted output: %s", chunk.Payload)
						}
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				} else {
					var result coreexecutor.Response
					result, err = exec.Execute(ctx, basispointsTestAuth(), req, opts)
					if len(result.Payload) != 0 {
						t.Fatal("failed response returned tool output")
					}
				}
				assertBasispointsRateLimit(t, err, 15*time.Millisecond)
				if calls.Load() != 1 {
					t.Fatalf("unexpected executor retries: %d", calls.Load())
				}
				if coreexecutor.IsCodexReplayUnsafe(err) != (transport == "websocket") {
					t.Fatal("existing websocket replay classification changed")
				}
			})
		}
	}
}

func TestBasispointsRequestRetryThree(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		for _, priorText := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior_text=%t", transport, priorText), func(t *testing.T) {
				cfg := &config.Config{RequestRetry: 3, Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: transport == "websocket"}}
				exec := NewCodexAutoExecutor(cfg)
				exec.basispointsExec.cooldown = basispoints.NewCooldown(time.Now)
				ctx, calls := basispointsFailureTransport(t, exec.basispointsExec, transport, priorText, "0")
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
				const model = "basispoints-terminal-retry"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				payload, _ := json.Marshal(map[string]any{"model": model, "input": "continue", "tools": []any{map[string]any{"type": "function", "name": "shell"}}})
				result, err := manager.ExecuteStream(ctx, []string{"codex"}, coreexecutor.Request{Model: model, Payload: payload}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
				var output bytes.Buffer
				if err == nil {
					for chunk := range result.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
				assertBasispointsRateLimit(t, err, 0)
				wantCalls := int32(1)
				if !priorText {
					wantCalls = 4
				}
				if calls.Load() != wantCalls {
					t.Fatalf("request-retry=3 made %d requests, want %d", calls.Load(), wantCalls)
				}
				if strings.Contains(output.String(), "run_officejs") {
					t.Fatal("partial tool leaked into client output")
				}
				if strings.Contains(output.String(), "already streamed") != priorText {
					t.Fatal("streamed text was lost or duplicated")
				}
			})
		}
	}
}

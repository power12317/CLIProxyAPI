package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func basispointsTestDial(target string) basispoints.WebsocketDial {
	return func(ctx context.Context, original string, headers http.Header, protocols []string) (*websocket.Conn, *http.Response, error) {
		u, err := url.Parse(original)
		if err != nil {
			return nil, nil, err
		}
		u.Scheme, u.Host = "ws", strings.TrimPrefix(target, "http://")
		dialer := websocket.Dialer{Subprotocols: protocols}
		return dialer.DialContext(ctx, u.String(), headers)
	}
}

func basispointsTestAuth() *coreauth.Auth {
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_account_user_id":"account-user"}}`)) + ".signature"
	return &coreauth.Auth{ID: "bps-ws-test", Provider: "codex", Metadata: map[string]any{"access_token": token, "account_id": "account"}}
}

func TestBasispointsWebsocketTransportSelectionAndToolConversion(t *testing.T) {
	for _, forced := range []bool{false, true} {
		for _, downstream := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("force=%t/downstream=%t/stream=%t", forced, downstream, stream), func(t *testing.T) {
					cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: forced}}
					exec := NewCodexAutoExecutor(cfg)
					auth := basispointsTestAuth()
					var wsCalls, sseCalls atomic.Int32
					const completed = `{"type":"response.completed","response":{"id":"resp_bps","model":"gpt-6-astra","status":"completed","output":[{"type":"function_call","id":"native_ws","call_id":"call_ws","name":"run_officejs","arguments":"{\"code\":\"{\\\"name\\\":\\\"weather\\\",\\\"arguments\\\":{\\\"city\\\":\\\"Paris\\\"}}\"}"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`
					checkBody := func(raw []byte) {
						if gjson.GetBytes(raw, "metadata.bps_tools_version_id").String() != basispoints.ToolsVersionID || gjson.GetBytes(raw, "reasoning_effort").String() != "low" {
							t.Errorf("wrong wire metadata or effort: %s", raw)
						}
						if gjson.GetBytes(raw, "context_management.0.compact_threshold").Int() != 400000 || gjson.GetBytes(raw, "prompt_cache_key").String() != "cache-key" || gjson.GetBytes(raw, "service_tier").Exists() {
							t.Errorf("existing optional fields changed: %s", raw)
						}
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						wsCalls.Add(1)
						if r.URL.Path != "/basispoints/api/responses" || r.URL.Query().Get("bps_auth_mode") != "chatgpt" || !strings.HasPrefix(r.URL.Query().Get("bps_ws_affinity"), "affinity_") || r.URL.Query().Get("bps_control_frames") != "upstream_sent,heartbeat,resume,cancel,server_draining" {
							t.Errorf("wrong native handshake URL: %s", r.URL)
						}
						if !strings.Contains(r.URL.Query().Get("bps_client_info"), "basispoints-excel-plugin") || r.Header.Get("X-OpenAI-Account-User-Id") != "account-user" {
							t.Error("missing client/account user identity")
						}
						protocols := websocket.Subprotocols(r)
						if len(protocols) != 2 || protocols[0] != "responses" || protocols[1] != "openai-bearer."+auth.Metadata["access_token"].(string) {
							t.Error("wrong bearer subprotocol")
						}
						upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
						conn, err := upgrader.Upgrade(w, r, nil)
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
						checkBody(raw)
						if gjson.GetBytes(raw, "type").String() != "response.create" || gjson.GetBytes(raw, "basispoints_request_id").String() == "" || !strings.HasPrefix(gjson.GetBytes(raw, "basispoints_resume_token").String(), "resume_") {
							t.Error("wrong native response.create envelope")
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"basispoints.response.heartbeat"}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
					}))
					defer server.Close()
					exec.basispointsExec.websocketDial = basispointsTestDial(server.URL)
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						sseCalls.Add(1)
						raw, _ := io.ReadAll(r.Body)
						checkBody(raw)
						if r.Header.Get("X-OpenAI-Account-User-Id") != "account-user" {
							t.Error("SSE lost the account user identity")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + completed + "\n\n"))}, nil
					})))
					if downstream {
						ctx = coreexecutor.WithDownstreamWebsocket(ctx)
					}
					var reported bool
					ctx = coreexecutor.WithUpstreamTransportCallback(ctx, func(ws bool) { reported = ws })
					req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"low"},"input":"hello","tools":[{"type":"function","name":"weather"}],"context_management":[{"type":"compaction","compact_threshold":400000}],"prompt_cache_key":"cache-key"}`)}
					opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
					var final []byte
					if stream {
						result, err := exec.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							if strings.Contains(string(chunk.Payload), "basispoints.response.heartbeat") {
								t.Fatal("control frame leaked into client stream")
							}
							if done := basispoints.CompletedResponse(chunk.Payload); len(done) > 0 {
								final = done
							}
						}
					} else {
						result, err := exec.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if result.Headers.Get("Upgrade") != "" || result.Headers.Get("Sec-WebSocket-Accept") != "" {
							t.Fatal("upstream handshake headers leaked into downstream response")
						}
						final = result.Payload
					}
					if gjson.GetBytes(final, "output.0.name").String() != "weather" || gjson.GetBytes(final, "output.0.encrypted_function_args").Raw != "[]" || !strings.Contains(gjson.GetBytes(final, "output.0.arguments").String(), "Paris") {
						t.Fatalf("tool conversion lost: %s", final)
					}
					wantWS := forced || downstream
					if reported != wantWS || (wsCalls.Load() == 1) != wantWS || (sseCalls.Load() == 1) == wantWS {
						t.Fatalf("route ws=%d sse=%d reported=%t", wsCalls.Load(), sseCalls.Load(), reported)
					}
				})
			}
		}
	}
}

func TestBasispointsWebsocketResumeUsesNativeTokenAndCursor(t *testing.T) {
	var connections atomic.Int32
	var affinity atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := connections.Add(1)
		upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
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
		if attempt == 1 {
			affinity.Store(r.URL.Query().Get("bps_ws_affinity"))
			id := gjson.GetBytes(raw, "basispoints_request_id").String()
			_ = conn.WriteJSON(map[string]any{"type": "basispoints.response.resume_token", "request_id": id, "resume_token": "resume_server_token"})
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"A","basispoints_replay_cursor":0}`))
			return
		}
		if attempt != 2 || affinity.Load() != r.URL.Query().Get("bps_ws_affinity") || gjson.GetBytes(raw, "type").String() != "basispoints.response.resume" || gjson.GetBytes(raw, "resume_token").String() != "resume_server_token" || gjson.GetBytes(raw, "after_cursor").Int() != 0 {
			t.Errorf("wrong recovery: %s", raw)
		}
		for _, frame := range []string{
			`{"type":"basispoints.response.resumed"}`,
			`{"type":"response.output_text.delta","delta":"A","basispoints_replay_cursor":0}`,
			`{"type":"response.output_text.delta","delta":"B","basispoints_replay_cursor":1}`,
			`{"type":"response.completed","basispoints_replay_cursor":2,"response":{"id":"resumed","status":"completed","output":[]}}`,
		} {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
		}
	}))
	defer server.Close()
	exec := NewBasispointsExecutor(&config.Config{Codex: config.CodexConfig{ForceWebsocket: true}})
	exec.websocketDial = basispointsTestDial(server.URL)
	result, err := exec.ExecuteStream(t.Context(), basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"hello"}`)}, coreexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var events strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		events.Write(chunk.Payload)
	}
	if connections.Load() != 2 || strings.Count(events.String(), `"delta":"A"`) != 1 || strings.Count(events.String(), `"delta":"B"`) != 1 || !strings.Contains(events.String(), "response.completed") {
		t.Fatalf("resume duplicated/lost output: %s", events.String())
	}
}

func TestBasispointsWebsocketHandshakeFallbackAnd403(t *testing.T) {
	for _, status := range []int{503, 403, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			exec := NewBasispointsExecutor(&config.Config{Codex: config.CodexConfig{ForceWebsocket: true}})
			now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
			exec.cooldown = basispoints.NewCooldown(func() time.Time { return now })
			dials, posts := 0, 0
			exec.websocketDial = func(context.Context, string, http.Header, []string) (*websocket.Conn, *http.Response, error) {
				dials++
				return nil, &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"upstream rejection"}}`))}, websocket.ErrBadHandshake
			}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
				posts++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"))}, nil
			})))
			_, err := exec.Execute(ctx, basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"hello"}`)}, coreexecutor.Options{})
			if status == 503 {
				if err != nil || dials != 5 || posts != 1 {
					t.Fatalf("fallback err=%v dials=%d posts=%d", err, dials, posts)
				}
				_, err = exec.Execute(ctx, basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"next"}`)}, coreexecutor.Options{})
				if err != nil || dials != 10 || posts != 2 {
					t.Fatal("next request did not try WebSocket again")
				}
			} else {
				var code interface{ StatusCode() int }
				if !errors.As(err, &code) || code.StatusCode() != status || dials != 1 || posts != 0 {
					t.Fatalf("rejection changed: %v dials=%d posts=%d", err, dials, posts)
				}
			}
			if status == 403 && !exec.cooldown.PausedUntil().Equal(now.Add(30*time.Minute)) {
				t.Fatal("WebSocket 403 did not pause Basispoints")
			}
		})
	}
}

func TestBasispointsWebsocketCancellationClosesConnection(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		close(started)
		_, _, _ = conn.ReadMessage()
		close(closed)
	}))
	defer server.Close()
	exec := NewBasispointsExecutor(&config.Config{Codex: config.CodexConfig{ForceWebsocket: true}})
	exec.websocketDial = basispointsTestDial(server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := exec.ExecuteStream(ctx, basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"hello"}`)}, coreexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	for range result.Chunks {
	}
	<-closed
}

func TestBasispointsWebsocketErrorsKeepLogsAndCooldown(t *testing.T) {
	for _, requestLog := range []bool{false, true} {
		for _, handshake := range []bool{false, true} {
			t.Run(fmt.Sprintf("logging=%t/handshake=%t", requestLog, handshake), func(t *testing.T) {
				const rejected = `{"type":"error","status":403,"error":{"message":"blocked by upstream usage policy"}}`
				var resume atomic.Value
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if handshake {
						w.WriteHeader(403)
						_, _ = io.WriteString(w, rejected)
						return
					}
					upgrader := websocket.Upgrader{Subprotocols: []string{"responses"}, CheckOrigin: func(*http.Request) bool { return true }}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, frame, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					resume.Store(gjson.GetBytes(frame, "basispoints_resume_token").String())
					_ = conn.WriteMessage(websocket.TextMessage, []byte(rejected))
				}))
				defer server.Close()
				cfg := &config.Config{Codex: config.CodexConfig{ForceWebsocket: true}}
				cfg.RequestLog = requestLog
				exec := NewBasispointsExecutor(cfg)
				exec.cooldown = basispoints.NewCooldown(time.Now)
				exec.websocketDial = basispointsTestDial(server.URL)
				dir := t.TempDir()
				logger := logging.NewFileRequestLogger(requestLog, dir, "", 10)
				router := gin.New()
				router.Use(logging.GinLogrusLogger(), middleware.RequestLoggingMiddleware(logger))
				router.POST("/v1/responses", func(c *gin.Context) {
					ctx := context.WithValue(c.Request.Context(), "gin", c)
					_, err := exec.Execute(ctx, basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"diagnostic-payload"}`)}, coreexecutor.Options{})
					var status interface{ StatusCode() int }
					if !errors.As(err, &status) || status.StatusCode() != 403 {
						t.Errorf("upstream rejection lost: %v", err)
						c.Status(500)
						return
					}
					if !handshake && !coreexecutor.IsCodexReplayUnsafe(err) {
						t.Error("in-flight failure must not restart generation")
					}
					c.Data(403, "application/json", []byte(err.Error()))
				})
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"diagnostic-payload"}`)))
				if recorder.Code != 403 || exec.cooldown.PausedUntil().IsZero() {
					t.Fatal("WebSocket rejection bypassed response/cooldown")
				}
				files, err := filepath.Glob(filepath.Join(dir, "*.log"))
				if err != nil || len(files) != 1 {
					t.Fatalf("missing failure log: %v %v", files, err)
				}
				content, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, wanted := range []string{rejected, "diagnostic-payload", "wss://bps.openai.com/basispoints/api/responses", "Method: GET"} {
					if !bytes.Contains(content, []byte(wanted)) {
						t.Fatalf("failure log lacks %q", wanted)
					}
				}
				if bytes.Contains(content, []byte(basispointsTestAuth().Metadata["access_token"].(string))) {
					t.Fatal("bearer subprotocol leaked into logs")
				}
				if token, ok := resume.Load().(string); ok && token != "" && bytes.Contains(content, []byte(token)) {
					t.Fatal("resume capability leaked into logs")
				}
			})
		}
	}
}

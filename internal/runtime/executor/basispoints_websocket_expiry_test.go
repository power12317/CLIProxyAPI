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

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsWebsocketConnectionLimitRenewal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"renew", "repeated_limit", "other_400", "after_output"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				var connections atomic.Int32
				frames := make(chan []byte, 4)
				oldClosed := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					n := connections.Add(1)
					defer func() {
						_ = conn.Close()
						if n == 1 {
							close(oldClosed)
						}
					}()
					for {
						_, frame, errRead := conn.ReadMessage()
						if errRead != nil {
							return
						}
						frames <- frame
						if n == 1 || mode == "repeated_limit" {
							_ = conn.WriteJSON(map[string]any{"type": "basispoints.response.resume_token", "request_id": gjson.GetBytes(frame, "basispoints_request_id").String(), "resume_token": "old-resume-token"})
							cursor := 0
							if mode == "after_output" {
								_ = conn.WriteJSON(map[string]any{"type": "response.created", "basispoints_replay_cursor": cursor, "response": map[string]any{"id": "already-started"}})
								cursor++
							}
							code := "websocket_connection_limit_reached"
							if mode == "other_400" {
								code = "invalid_image"
							}
							_ = conn.WriteJSON(map[string]any{"type": "error", "status": 400, "basispoints_replay_cursor": cursor, "error": map[string]any{"type": "invalid_request_error", "code": code, "message": "connection or request rejected"}})
							continue
						}
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "basispoints_replay_cursor": 0, "response": map[string]any{"id": "fresh-response", "status": "completed", "output": []any{}}})
					}
				}))
				defer server.Close()
				cfg := &config.Config{RequestRetry: 0, Codex: config.CodexConfig{ForceWebsocket: true}}
				cfg.RequestLog = true
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				ctx := context.WithValue(t.Context(), "gin", ginCtx)
				exec := NewBasispointsExecutor(cfg)
				isolateBasispointsWebsockets(t, exec)
				exec.websocketDial = basispointsTestDial(server.URL)
				auth := basispointsTestAuth()
				auth.ID = t.Name()
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-sol", "input": "preserve the full request", "reasoning": map[string]any{"effort": "low"}, "client_metadata": map[string]any{"session_id": "expiry-session", "turn_id": "expiry-turn"}})
				req := coreexecutor.Request{Model: "gpt-6-sol", Payload: body}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				run := func() ([]byte, error) {
					if !stream {
						result, err := exec.Execute(ctx, auth, req, opts)
						return result.Payload, err
					}
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						return nil, err
					}
					var output bytes.Buffer
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
						output.Write(chunk.Payload)
					}
					return output.Bytes(), err
				}
				output, err := run()
				wantConnections := int32(1)
				if mode == "renew" || mode == "repeated_limit" {
					wantConnections = 2
				}
				if got := connections.Load(); got != wantConnections {
					t.Fatalf("connections=%d want=%d, err=%v", got, wantConnections, err)
				}
				if mode != "renew" {
					if clienterror.HTTPStatusFromError(err) != 400 {
						t.Fatalf("original rejection lost: %v", err)
					}
					return
				}
				if err != nil || !bytes.Contains(output, []byte("fresh-response")) || bytes.Contains(output, []byte("websocket_connection_limit_reached")) {
					t.Fatalf("renewal failed or leaked rejection: %s %v", output, err)
				}
				<-oldClosed
				first, second := <-frames, <-frames
				requestLog := string(ginCtx.MustGet("API_REQUEST").([]byte))
				responseLog := string(ginCtx.MustGet("API_RESPONSE").([]byte))
				for _, frame := range [][]byte{first, second} {
					if !strings.Contains(requestLog, gjson.GetBytes(frame, "basispoints_request_id").String()) {
						t.Fatal("renewal request missing from upstream log")
					}
					if strings.Contains(requestLog, gjson.GetBytes(frame, "basispoints_resume_token").String()) {
						t.Fatal("renewal log leaked resume token")
					}
				}
				if !strings.Contains(responseLog, "websocket_connection_limit_reached") {
					t.Fatal("renewal hid the upstream rejection from logs")
				}
				var before, after map[string]any
				_ = json.Unmarshal(first, &before)
				_ = json.Unmarshal(second, &after)
				for _, field := range []string{"basispoints_request_id", "basispoints_resume_token"} {
					if before[field] == after[field] || after[field] == "" {
						t.Fatalf("stale protocol identity: %s", field)
					}
					delete(before, field)
					delete(after, field)
				}
				if !reflect.DeepEqual(before, after) || after["type"] != "response.create" {
					t.Fatal("renewal changed the request or sent resume instead of response.create")
				}
				if _, err = run(); err != nil || connections.Load() != 2 {
					t.Fatalf("replacement connection was not retained: %v", err)
				}
			})
		}
	}
}

func TestBasispointsConnectionLimitRenewalHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "error", "status": 400, "error": map[string]any{"code": "websocket_connection_limit_reached"}})
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	exec := NewBasispointsExecutor(&config.Config{Codex: config.CodexConfig{ForceWebsocket: true}})
	isolateBasispointsWebsockets(t, exec)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reconnecting := make(chan struct{})
	dial := basispointsTestDial(server.URL)
	var calls atomic.Int32
	exec.websocketDial = func(ctx context.Context, target string, headers http.Header, protocols []string) (*websocket.Conn, *http.Response, error) {
		if calls.Add(1) == 2 {
			close(reconnecting)
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return dial(ctx, target, headers, protocols)
	}
	done := make(chan error, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"input": "hello"})
		_, err := exec.Execute(ctx, basispointsTestAuth(), coreexecutor.Request{Model: "gpt-6-sol", Payload: body}, coreexecutor.Options{})
		done <- err
	}()
	<-reconnecting
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || calls.Load() != 2 {
		t.Fatalf("renewal ignored cancellation: %v calls=%d", err, calls.Load())
	}
}

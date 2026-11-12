package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestBasispointsWebsocketCancellationSendsNativeControlFrame(t *testing.T) {
	controls := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"responses"}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, create, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		requestID := ojson.GetBytes(create, "basispoints_request_id").String()
		_ = conn.WriteJSON(map[string]any{"type": "basispoints.response.resume_token", "request_id": requestID, "resume_token": "server-resume-token"})
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"ready","basispoints_replay_cursor":0}`))
		_, cancelFrame, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		if ojson.GetBytes(cancelFrame, "request_id").String() != requestID {
			t.Error("cancel used a different generation")
		}
		controls <- cancelFrame
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	exec := NewBasispointsExecutor(forceWebsocketTestConfig())
	isolateBasispointsWebsockets(t, exec)
	exec.websocketDial = basispointsTestDial(server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := exec.ExecuteStream(ctx, basispointsTestAuth(), core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	chunk := <-result.Chunks
	if chunk.Err != nil || !strings.Contains(string(chunk.Payload), "ready") {
		t.Fatalf("unexpected initial event: %s %v", chunk.Payload, chunk.Err)
	}
	cancel()
	for range result.Chunks {
	}
	frame := <-controls
	if ojson.GetBytes(frame, "type").String() != "basispoints.response.cancel" || ojson.GetBytes(frame, "resume_token").String() != "server-resume-token" {
		t.Fatalf("wrong cancel frame: %s", frame)
	}
}

func TestBasispointsWebsocketInitialDrainRetriesWithNewAffinity(t *testing.T) {
	for _, drain := range []struct{ name, event string }{
		{"flat-error", `{"type":"error","code":"basispoints_server_draining","message":"worker restarting"}`},
		{"nested-error", `{"type":"error","error":{"code":"basispoints_server_draining","message":"worker restarting"}}`},
		{"named-event", `{"type":"basispoints.response.server_draining"}`},
	} {
		t.Run(drain.name, func(t *testing.T) {

			var connections atomic.Int32
			queries := make(chan string, 2)
			frames := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := connections.Add(1)
				queries <- r.URL.Query().Get("bps_ws_affinity")
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"responses"}}
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
				frames <- raw
				if attempt == 1 {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(drain.event))
					_, _, _ = conn.ReadMessage()
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`))
			}))
			defer server.Close()
			cfg := forceWebsocketTestConfig()
			cfg.RequestRetry = 1
			exec := NewBasispointsExecutor(cfg)
			isolateBasispointsWebsockets(t, exec)
			exec.websocketDial = basispointsTestDial(server.URL)
			_, err := exec.Execute(t.Context(), basispointsTestAuth(), core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, core.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 2 {
				t.Fatalf("connections=%d", connections.Load())
			}
			firstAffinity, secondAffinity := <-queries, <-queries
			if firstAffinity == secondAffinity {
				t.Fatal("draining did not rotate affinity")
			}
			first, second := <-frames, <-frames
			if ojson.GetBytes(first, "metadata.turn_id").String() != ojson.GetBytes(second, "metadata.turn_id").String() || ojson.GetBytes(second, "type").String() != "response.create" || ojson.GetBytes(first, "basispoints_request_id").String() == ojson.GetBytes(second, "basispoints_request_id").String() {
				t.Fatal("draining retry changed the business turn or reused the generation identity")
			}

		})
	}
}

func TestBasispointsWebsocketRecoveryErrorsDoNotRegenerate(t *testing.T) {
	for _, tc := range []struct {
		code         string
		safe, rotate bool
	}{
		{"basispoints_server_draining", false, true},
		{"basispoints.response.server_draining", false, true},
		{"basispoints_resume_not_found", false, false},
		{"basispoints_resume_not_found", true, true},
		{"basispoints_resume_gap", false, false},
		{"basispoints_resume_invalid", false, false},
		{"basispoints_resume_already_attached", false, false},
	} {
		t.Run(fmt.Sprintf("%s/safe=%v", tc.code, tc.safe), func(t *testing.T) {
			var connections atomic.Int32
			affinities := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := connections.Add(1)
				affinities <- r.URL.Query().Get("bps_ws_affinity")
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"responses"}}
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
				switch attempt {
				case 1:
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial","basispoints_replay_cursor":0}`))
				case 2:
					if ojson.GetBytes(raw, "type").String() != "basispoints.response.resume" || ojson.GetBytes(raw, "after_cursor").Int() != 0 {
						t.Error("generation was restarted instead of resumed")
					}
					if tc.code == "basispoints.response.server_draining" {
						_ = conn.WriteJSON(map[string]any{"type": tc.code})
					} else {
						_ = conn.WriteJSON(map[string]any{"type": "error", "code": tc.code, "message": "recovery failed", "error": map[string]any{"safe_to_rotate_affinity": tc.safe}})
					}
					_, _, _ = conn.ReadMessage()
				case 3:
					if ojson.GetBytes(raw, "type").String() != "response.create" {
						t.Error("new request did not create a response")
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"next","status":"completed","output":[]}}`))
				default:
					t.Error("unexpected regeneration")
				}
			}))
			defer server.Close()
			cfg := forceWebsocketTestConfig()
			cfg.RequestRetry = 3
			exec := NewBasispointsExecutor(cfg)
			isolateBasispointsWebsockets(t, exec)
			exec.websocketDial = basispointsTestDial(server.URL)
			req := core.Request{Model: "test-model", Payload: []byte(`{"input":"hello","client_metadata":{"session_id":"controls-session"}}`)}
			_, err := exec.Execute(t.Context(), basispointsTestAuth(), req, core.Options{})
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("lost recovery error: %v", err)
			}
			if connections.Load() != 2 {
				t.Fatal("recovery failure regenerated the request")
			}
			if _, err = exec.Execute(t.Context(), basispointsTestAuth(), req, core.Options{}); err != nil {
				t.Fatal(err)
			}
			first, resumed, next := <-affinities, <-affinities, <-affinities
			if first != resumed || (next != resumed) != tc.rotate {
				t.Fatal("affinity rotation did not follow explicit recovery permission")
			}
		})
	}
}

func TestBasispointsWebsocketDispatchedDrainDoesNotRetry(t *testing.T) {
	for _, drain := range []struct{ name, event string }{
		{"flat-error", `{"type":"error","code":"basispoints_server_draining","message":"worker restarting"}`},
		{"nested-error", `{"type":"error","error":{"code":"basispoints_server_draining","message":"worker restarting"}}`},
		{"named-event", `{"type":"basispoints.response.server_draining"}`},
	} {
		t.Run(drain.name, func(t *testing.T) {

			var connections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _, _ = conn.ReadMessage()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"basispoints.response.upstream_sent"}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(drain.event))
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			cfg := forceWebsocketTestConfig()
			cfg.RequestRetry = 3
			exec := NewBasispointsExecutor(cfg)
			isolateBasispointsWebsockets(t, exec)
			exec.websocketDial = basispointsTestDial(server.URL)
			_, err := exec.Execute(t.Context(), basispointsTestAuth(), core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, core.Options{})
			if err == nil || !strings.Contains(err.Error(), "draining") || connections.Load() != 1 {
				t.Fatalf("dispatched generation was replayed: calls=%d err=%v", connections.Load(), err)
			}

		})
	}
}

func TestBasispointsWebsocketIdleDrainRetiresConnection(t *testing.T) {
	for _, drain := range []struct{ name, event string }{
		{"flat-error", `{"type":"error","code":"basispoints_server_draining","message":"worker restarting"}`},
		{"nested-error", `{"type":"error","error":{"code":"basispoints_server_draining","message":"worker restarting"}}`},
		{"named-event", `{"type":"basispoints.response.server_draining"}`},
	} {
		t.Run(drain.name, func(t *testing.T) {

			completed, retired := make(chan struct{}), make(chan struct{})
			var connections atomic.Int32
			affinities := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := connections.Add(1)
				affinities <- r.URL.Query().Get("bps_ws_affinity")
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
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`))
				if attempt == 1 {
					<-completed
					_ = conn.WriteMessage(websocket.TextMessage, []byte(drain.event))
					_, _, _ = conn.ReadMessage()
					close(retired)
				}
			}))
			defer server.Close()
			exec := NewBasispointsExecutor(forceWebsocketTestConfig())
			isolateBasispointsWebsockets(t, exec)
			exec.websocketDial = basispointsTestDial(server.URL)
			req := core.Request{Model: "test-model", Payload: []byte(`{"input":"hello","client_metadata":{"session_id":"idle-drain"}}`)}
			if _, err := exec.Execute(t.Context(), basispointsTestAuth(), req, core.Options{}); err != nil {
				t.Fatal(err)
			}
			close(completed)
			<-retired
			if _, err := exec.Execute(t.Context(), basispointsTestAuth(), req, core.Options{}); err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 2 || <-affinities == <-affinities {
				t.Fatal("idle draining connection was reused")
			}

		})
	}
}

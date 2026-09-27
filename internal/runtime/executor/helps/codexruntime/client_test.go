package codexruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gorilla/websocket"
)

func serveBridge(t *testing.T, caps Capabilities, inference func(*websocket.Conn, message)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cpa-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var m message
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			switch m.Method {
			case "initialize":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{}})
			case "initialized":
			case "cpa/capabilities/read":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": caps})
			case "cpa/inference/start":
				inference(conn, m)
				return
			default:
				t.Errorf("unexpected method %s", m.Method)
				return
			}
		}
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { _ = server.Close(); _ = os.Remove(socket); _ = os.Remove(dir) })
	return socket
}
func validCaps() Capabilities {
	return Capabilities{ProtocolVersion: 1, RuntimeVersion: "test", UpstreamRevision: "test-sha", CredentialID: "worker", AccountID: "account", AuthMode: "chatgpt", ExecutionMode: "inference-only", RawEvents: true, Operations: []string{"responses"}}
}
func request() Request {
	return Request{RequestID: "req", CredentialID: "worker", AccountID: "account", Operation: "responses", SourceFormat: "openai-response", SessionID: "session", Request: json.RawMessage(`{"model":"test","input":[]}`)}
}
func accept(conn *websocket.Conn, m message) {
	_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{"requestId": "req", "statusCode": 200, "headers": map[string][]string{"Set-Cookie": {"secret"}, "Content-Type": {"text/event-stream"}, "X-Request-ID": {"upstream"}, "Authorization": {"secret"}}}})
}
func notify(conn *websocket.Conn, method string, params any) {
	_ = conn.WriteJSON(map[string]any{"method": method, "params": params})
}

func TestWireEventFidelityAndHeaderBoundary(t *testing.T) {
	raw := json.RawMessage(`{"type":"response.future_extension","unknown":{"a":[1,true]},"delta":"part"}`)
	socket := serveBridge(t, validCaps(), func(c *websocket.Conn, m message) {
		accept(c, m)
		notify(c, "cpa/inference/event", map[string]any{"requestId": "req", "event": raw})
		notify(c, "cpa/inference/event", map[string]any{"requestId": "req", "event": json.RawMessage(`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"new_field":42}}`)})
		notify(c, "cpa/inference/completed", map[string]string{"requestId": "req"})
	})
	c, err := Open(context.Background(), socket, "worker", "account")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Start(request()); err != nil {
		t.Fatal(err)
	}
	if c.Headers.Get("Set-Cookie") != "" || c.Headers.Get("Authorization") != "" || c.Headers.Get("X-Request-ID") != "upstream" {
		t.Fatal("unsafe headers", c.Headers)
	}
	frame, err := c.Next()
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	_ = json.Unmarshal(raw, &want)
	_ = json.Unmarshal(frame.Event, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatal("wire event lost fields")
	}
	if _, err = c.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Next(); err != io.EOF {
		t.Fatalf("got %v, want EOF", err)
	}
}
func TestRejectCapabilitiesBeforeInference(t *testing.T) {
	for _, change := range []func(*Capabilities){func(c *Capabilities) { c.ProtocolVersion = 2 }, func(c *Capabilities) { c.AccountID = "other" }, func(c *Capabilities) { c.CredentialID = "other" }, func(c *Capabilities) { c.AuthMode = "chatgptAuthTokens" }, func(c *Capabilities) { c.ExecutionMode = "agent" }, func(c *Capabilities) { c.RawEvents = false }} {
		caps := validCaps()
		change(&caps)
		socket := serveBridge(t, caps, func(*websocket.Conn, message) { t.Error("inference must not start") })
		c, err := Open(context.Background(), socket, "worker", "account")
		if c != nil {
			c.Close()
		}
		if err == nil {
			t.Fatal("accepted mismatched capabilities")
		}
	}
}
func TestIncompleteBridgeStreamIsAnError(t *testing.T) {
	for _, mode := range []string{"no-terminal", "disconnect", "wrong-request", "after-terminal"} {
		t.Run(mode, func(t *testing.T) {
			socket := serveBridge(t, validCaps(), func(c *websocket.Conn, m message) {
				accept(c, m)
				if mode == "disconnect" {
					return
				}
				if mode == "wrong-request" {
					notify(c, "cpa/inference/event", map[string]any{"requestId": "other", "event": map[string]string{"type": "response.created"}})
					return
				}
				if mode == "after-terminal" {
					for range 2 {
						notify(c, "cpa/inference/event", map[string]any{"requestId": "req", "event": json.RawMessage(`{"type":"response.completed","response":{"id":"r"}}`)})
					}
					return
				}
				notify(c, "cpa/inference/completed", map[string]string{"requestId": "req"})
			})
			c, err := Open(context.Background(), socket, "worker", "account")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err = c.Start(request()); err != nil {
				t.Fatal(err)
			}
			if mode == "after-terminal" {
				if _, err = c.Next(); err != nil {
					t.Fatal(err)
				}
			}
			_, err = c.Next()
			var status *Error
			if !errors.As(err, &status) || !status.IsRequestScoped() {
				t.Fatalf("got %v, want scoped error", err)
			}
		})
	}
}
func TestCancellationClosesOnlyRequestConnection(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	socket := serveBridge(t, validCaps(), func(c *websocket.Conn, m message) {
		accept(c, m)
		close(started)
		_, _, _ = c.ReadMessage()
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := Open(ctx, socket, "worker", "account")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Start(request()); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	<-closed
	if _, err = c.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
func TestUnsupportedContinuationNeverSubmitted(t *testing.T) {
	socket := serveBridge(t, validCaps(), func(*websocket.Conn, message) { t.Error("unsupported inference submitted") })
	c, err := Open(context.Background(), socket, "worker", "account")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, body := range []string{`{"previous_response_id":"old"}`, `{"generate":false}`} {
		r := request()
		r.Request = json.RawMessage(body)
		if c.Start(r) == nil {
			t.Fatal("unsupported continuation accepted")
		}
	}
}

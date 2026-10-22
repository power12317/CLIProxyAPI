package codexruntime

import (
	"io"
	"testing"

	"github.com/gorilla/websocket"
)

func TestOptionalNotificationsAndBadDiagnosticsDoNotFailModel(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, rpc message) {
		for range 256 {
			notify(conn, "runtime/status", map[string]any{"optional": true})
			notify(conn, "cpa/inference/progress", map[string]any{"future": true})
		}
		notify(conn, "cpa/inference/upstream", map[string]any{"requestId": "other", "kind": "request", "body": "must not enter this request's logs"})
		notify(conn, "cpa/inference/upstream", map[string]any{"requestId": "req", "kind": "future-kind"})
		notify(conn, "cpa/inference/upstream", map[string]any{"requestId": "req", "kind": "body", "headers": "invalid optional diagnostic"})
		notify(conn, "cpa/inference/diagnostics/completed", map[string]any{"requestId": "other"})
		accept(conn, rpc)
		for range 256 {
			notify(conn, "cpa/inference/optional", map[string]any{"future": []string{"not", "business"}})
		}
		complete(conn, []byte("delivered model event"))
	})
	client := dialMaster(t, endpoint)
	client.OnUpstream = func(entry UpstreamLog) { t.Errorf("unrelated diagnostic delivered: %+v", entry) }
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	frame, err := client.Next()
	if err != nil || string(frame.Body) != "delivered model event" {
		t.Fatalf("body=%q err=%v", frame.Body, err)
	}
	if _, err := client.Next(); err != io.EOF || !client.NativeCompleted() {
		t.Fatalf("native completion lost: %v", err)
	}
	client.WaitDiagnostics()
}

func TestOptionalNotificationsBeforeManagementResult(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, rpc message) {
		for range 512 {
			notify(conn, "runtime/status", map[string]any{"optional": true})
		}
		_ = conn.WriteJSON(map[string]any{"id": rpc.ID, "result": map[string]any{"accepted": true, "newOptional": "metadata"}})
	})
	client := dialMaster(t, endpoint)
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if err := client.Call("test/status", map[string]any{}, &result); err != nil || !result.Accepted {
		t.Fatalf("accepted=%t err=%v", result.Accepted, err)
	}
}

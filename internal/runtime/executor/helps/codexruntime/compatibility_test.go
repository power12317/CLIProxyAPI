package codexruntime

import (
	"io"
	"net/http"
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
		accept(conn, rpc)
		for range 256 {
			notify(conn, "cpa/inference/optional", map[string]any{"future": []string{"not", "business"}})
		}
		sendBody(conn, []byte("delivered model event"))
		notify(conn, "cpa/inference/completed", map[string]any{"requestId": "req"})
	})
	client := dialMaster(t, endpoint)
	client.OnUpstream = func(entry UpstreamLog) {
		if entry.Kind != "body" {
			t.Errorf("unrelated diagnostic delivered: %+v", entry)
		}
	}
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	frame, err := client.Next()
	if err != nil || string(frame.Body) != "delivered model event" {
		t.Fatalf("body=%q err=%v", frame.Body, err)
	}
	if _, err := client.Next(); err != io.EOF {
		t.Fatalf("completion lost: %v", err)
	}
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

func TestSafeHeadersPreserveSafetyBufferingMetadata(t *testing.T) {
	headers := SafeHeaders(http.Header{
		"x-codex-safety-buffering-enabled":      {"true"},
		"x-codex-safety-buffering-faster-model": {"gpt-6-luna"},
		"authorization":                         {"private"},
		"set-cookie":                            {"private"},
	})
	if headers.Get("X-Codex-Safety-Buffering-Enabled") != "true" || headers.Get("X-Codex-Safety-Buffering-Faster-Model") != "gpt-6-luna" {
		t.Fatalf("model response metadata was lost: %v", headers)
	}
	if headers.Get("Authorization") != "" || headers.Get("Set-Cookie") != "" {
		t.Fatal("private transport headers escaped")
	}
}

package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func muxTestDial(t *testing.T, serve func(*websocket.Conn)) (CodexWebsocketDial, *atomic.Int32) {
	t.Helper()
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Close the server side during cleanup, never the registry-owned client.
		t.Cleanup(func() { _ = conn.Close() })
		serve(conn)
	}))
	t.Cleanup(server.Close)
	return func(ctx context.Context) (*websocket.Conn, *http.Response, error) {
		connections.Add(1)
		return websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	}, &connections
}

func TestCodexMuxCapacityAndLaneReuseNeverDialSecondConnection(t *testing.T) {
	ack := make(chan struct{})
	dial, connections := muxTestDial(t, func(conn *websocket.Conn) {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			stream := gjson.GetBytes(payload, "stream_id").String()
			response := fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":"r","output":[]}}`, stream)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(response)); err != nil {
				return
			}
			ack <- struct{}{}
		}
	})
	registry := &CodexWebsocketRegistry{}
	target := CodexWebsocketTarget{Credential: "one", Owner: "owner", URL: "upstream"}
	lanes := make([]*CodexWebsocketLane, codexWebsocketLaneLimit)
	for i := range lanes {
		target.Affinity = fmt.Sprintf("session-%d", i)
		lane, _, err := registry.Acquire(t.Context(), target, dial)
		if err != nil {
			t.Fatal(err)
		}
		lanes[i] = lane
	}
	if _, _, err := registry.Acquire(t.Context(), target, dial); err == nil {
		t.Fatal("capacity exhaustion must not create another connection")
	}
	for _, lane := range lanes {
		lane.Release()
	}
	// More than 32 distinct conversations reuse the fixed stream namespace.
	seen := map[string]bool{}
	for i := range 96 {
		target.Affinity = fmt.Sprintf("new-session-%d", i)
		lane, _, err := registry.Acquire(t.Context(), target, dial)
		if err != nil {
			t.Fatal(err)
		}
		seen[lane.StreamID()] = true
		err = lane.Write([]byte(`{"type":"response.create","input":[]}`), func(payload []byte) error { return lane.Conn().WriteMessage(websocket.TextMessage, payload) })
		if err != nil {
			t.Fatal(err)
		}
		<-ack
		if _, err := lane.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		lane.Release()
	}
	if connections.Load() != 1 || len(seen) > codexWebsocketLaneLimit {
		t.Fatalf("connections=%d named streams=%d", connections.Load(), len(seen))
	}
}

func TestCodexMuxDifferentCredentialsAreIndependent(t *testing.T) {
	dial, connections := muxTestDial(t, func(conn *websocket.Conn) { _, _, _ = conn.ReadMessage() })
	registry := &CodexWebsocketRegistry{}
	for _, credential := range []string{"one", "two", "one", "two"} {
		lane, _, err := registry.Acquire(t.Context(), CodexWebsocketTarget{Credential: credential, Owner: "same-account", URL: "upstream"}, dial)
		if err != nil {
			t.Fatal(err)
		}
		lane.Release()
	}
	if connections.Load() != 2 {
		t.Fatalf("connections=%d, want one per credential", connections.Load())
	}
}

func TestCodexMuxOwnerChangeDoesNotCloseOrReplaceLiveSocket(t *testing.T) {
	dial, connections := muxTestDial(t, func(conn *websocket.Conn) { _, _, _ = conn.ReadMessage() })
	registry := &CodexWebsocketRegistry{}
	target := CodexWebsocketTarget{Credential: "one", Owner: "original", URL: "upstream"}
	lane, _, err := registry.Acquire(t.Context(), target, dial)
	if err != nil {
		t.Fatal(err)
	}
	lane.Release()
	changed := target
	changed.Owner = "different-account"
	if _, _, err := registry.Acquire(t.Context(), changed, dial); err == nil {
		t.Fatal("a different account must not use an existing authenticated connection")
	}
	reused, _, err := registry.Acquire(t.Context(), target, dial)
	if err != nil || reused.Conn() != lane.Conn() || connections.Load() != 1 {
		t.Fatalf("healthy connection replaced: connections=%d err=%v", connections.Load(), err)
	}
	reused.Release()
}

func TestCodexMuxSlowSubscriberDoesNotBlockOtherLane(t *testing.T) {
	ready := make(chan struct{})
	dial, connections := muxTestDial(t, func(conn *websocket.Conn) {
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_, second, err := conn.ReadMessage()
		if err != nil {
			return
		}
		for range 300 {
			payload := fmt.Sprintf(`{"type":"response.output_text.delta","stream_id":%q,"delta":"x"}`, gjson.GetBytes(first, "stream_id").String())
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				return
			}
		}
		for _, frame := range [][]byte{first, second} {
			payload := fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":"r","output":[]}}`, gjson.GetBytes(frame, "stream_id").String())
			_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
		}
		close(ready)
		_, _, _ = conn.ReadMessage()
	})
	registry := &CodexWebsocketRegistry{}
	target := CodexWebsocketTarget{Credential: "one", Owner: "owner", URL: "upstream"}
	first, _, err := registry.Acquire(t.Context(), target, dial)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := registry.Acquire(t.Context(), target, dial)
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range []*CodexWebsocketLane{first, second} {
		if err := lane.Write([]byte(`{"type":"response.create"}`), func(payload []byte) error { return lane.Conn().WriteMessage(websocket.TextMessage, payload) }); err != nil {
			t.Fatal(err)
		}
	}
	<-ready
	if event, err := second.Read(t.Context()); err != nil || gjson.GetBytes(event, "type").String() != "response.completed" {
		t.Fatalf("healthy subscriber blocked: %s %v", event, err)
	}
	if connections.Load() != 1 {
		t.Fatal("slow reader caused another connection")
	}
	first.Release()
	second.Release()
}

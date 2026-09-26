package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketOverloadDoesNotCloseDownstream(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.Close() }()
				writer := newResponsesWebsocketWriter(conn)
				errMsg := &interfaces.ErrorMessage{StatusCode: 503, Error: fmt.Errorf(`{"error":{"code":"server_is_overloaded","message":"busy","type":"server_error"}}`)}
				var rawPayload []byte
				if raw {
					rawPayload = []byte(`{"type":"error","status":503,"error":{"code":"server_is_overloaded","message":"busy","type":"server_error"}}`)
				}
				_, wrote, err := writeResponsesWebsocketTerminalError(writer, nil, errMsg, rawPayload)
				if err != nil || !wrote {
					done <- fmt.Errorf("overload write: wrote=%t err=%v", wrote, err)
					return
				}
				if _, _, err := conn.ReadMessage(); err != nil {
					done <- err
					return
				}
				done <- conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"next","output":[]}}`))
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_, payload, err := conn.ReadMessage()
			if err != nil || gjson.GetBytes(payload, "error.code").String() != "server_is_overloaded" {
				t.Fatalf("overload was not delivered: %s %v", payload, err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			_, payload, err = conn.ReadMessage()
			if err != nil || gjson.GetBytes(payload, "response.id").String() != "next" {
				t.Fatalf("follow-up failed: %s %v", payload, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

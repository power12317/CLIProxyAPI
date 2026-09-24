package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexOAuthWebsocketHandshakeAndPrewarmFidelity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, overrides := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/overrides=%t", stream, overrides), func(t *testing.T) {
				headers := make(chan http.Header, 1)
				frames := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers <- r.Header.Clone()
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, frame, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Error(errRead)
						return
					}
					frames <- frame
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"warmup","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
				}))
				defer server.Close()
				cfg := &config.Config{Codex: config.CodexConfig{ForceWebsocket: true}, SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"}}
				if overrides {
					auth.Attributes["header:Accept"] = "custom/accept"
					auth.Attributes["header:Conversation_id"] = "custom-conversation"
					auth.Attributes["header:"+codexResponsesLiteHeader] = "true"
				}
				body := []byte(`{"model":"gpt-5.6-terra","generate":false,"input":[],"prompt_cache_key":"fixture-session","client_metadata":{"turn_id":"","ws_request_header_x_openai_internal_codex_responses_lite":"true","x-codex-turn-metadata":"{\"session_id\":\"fixture-session\",\"thread_id\":\"fixture-thread\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"}}`)
				req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body}
				executor := NewCodexWebsocketsExecutor(cfg)
				ctx := cliproxyexecutor.WithCodexTransport(t.Context(), &cliproxyexecutor.CodexTransportState{})
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				gotHeaders, frame := <-headers, <-frames
				for name, configured := range map[string]string{"Accept": "custom/accept", "Conversation_id": "custom-conversation", codexResponsesLiteHeader: "true"} {
					want := ""
					if overrides {
						want = configured
					}
					if got := gotHeaders.Get(name); got != want {
						t.Errorf("%s=%q, want %q", name, got, want)
					}
				}
				if gotHeaders.Get("Session-Id") != "fixture-session" || gotHeaders.Get("Authorization") != "Bearer fixture-token" || gotHeaders.Get("OpenAI-Beta") != codexResponsesWebsocketBetaHeaderValue {
					t.Errorf("missing required handshake fields: %v", gotHeaders)
				}
				if gjson.GetBytes(frame, "client_metadata.turn_id").String() != "" || gjson.Get(gjson.GetBytes(frame, "client_metadata.x-codex-turn-metadata").String(), "turn_id").String() != "" {
					t.Errorf("prewarm gained turn identity: %s", frame)
				}
				if gjson.GetBytes(frame, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String() != "true" || gjson.GetBytes(frame, "generate").Type != gjson.False {
					t.Errorf("lost frame-only settings: %s", frame)
				}
			})
		}
	}
}

type codexHeartbeatDeadlineConn struct {
	net.Conn
	deadlines chan time.Time
}

func (c *codexHeartbeatDeadlineConn) SetReadDeadline(deadline time.Time) error {
	err := c.Conn.SetReadDeadline(deadline)
	c.deadlines <- deadline
	return err
}

func TestCodexWebsocketHeartbeatRefreshesReadDeadline(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("pooled=%t", pooled), func(t *testing.T) {
			peers := make(chan *websocket.Conn, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err == nil {
					peers <- conn
				}
			}))
			defer server.Close()
			var spy *codexHeartbeatDeadlineConn
			dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				spy = &codexHeartbeatDeadlineConn{Conn: conn, deadlines: make(chan time.Time, 16)}
				return spy, nil
			}}
			client, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			peer := <-peers
			defer func() { _ = peer.Close() }()
			for len(spy.deadlines) > 0 {
				<-spy.deadlines
			}
			var session *codexWebsocketSession
			if pooled {
				session = &codexWebsocketSession{sessionID: t.Name(), conn: client, connCloser: newWebsocketConnectionCloser(client)}
				session.configureConn(client)
			} else {
				configureRawCodexWebsocketConn(client, "fixture", server.URL)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if pooled {
					NewCodexWebsocketsExecutor(&config.Config{}).readUpstreamLoop(session, client)
				} else {
					_, _, _ = readCodexWebsocketMessage(t.Context(), nil, client, nil)
				}
			}()
			awaitDeadline := func() time.Time {
				t.Helper()
				select {
				case deadline := <-spy.deadlines:
					return deadline
				case <-time.After(5 * time.Second):
					t.Fatal("heartbeat did not refresh the deadline")
					return time.Time{}
				}
			}
			initial := awaitDeadline()
			pongs := make(chan string, 1)
			peer.SetPongHandler(func(data string) error { pongs <- data; return nil })
			peerDone := make(chan struct{})
			go func() { defer close(peerDone); _, _, _ = peer.ReadMessage() }()
			for _, kind := range []int{websocket.PingMessage, websocket.PongMessage} {
				before := time.Now()
				if errWrite := peer.WriteControl(kind, []byte("heartbeat"), time.Time{}); errWrite != nil {
					t.Fatal(errWrite)
				}
				if deadline := awaitDeadline(); deadline.Before(initial) || deadline.Before(before.Add(codexResponsesWebsocketIdleTimeout)) {
					t.Fatalf("deadline was not renewed: %v", deadline)
				}
			}
			select {
			case pong := <-pongs:
				if pong != "heartbeat" {
					t.Fatalf("pong=%q", pong)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing pong reply")
			}
			_ = client.Close()
			_ = peer.Close()
			<-done
			<-peerDone
		})
	}
}

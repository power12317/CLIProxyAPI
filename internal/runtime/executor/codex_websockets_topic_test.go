package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTopicReusesAcrossConnectionsTurnsModelsAndReload(t *testing.T) {
	var connections atomic.Int32
	frames := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			frames <- payload
			name := gjson.GetBytes(payload, "input.0.content").String()
			if name == "overload" {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":503,"error":{"code":"server_is_overloaded","message":"busy"}}`))
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"fallback-state"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"standard-state"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, name)))
		}
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
	t.Cleanup(func() { e.CloseExecutionSession(auth.CloseAllExecutionSessionsID) })
	credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "key", "account_id": "topic-owner", "codex_client_system": "windows"}}
	execute := func(name, topic, turn, model, downstream string, overload bool) []byte {
		t.Helper()
		req := core.Request{Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":%q}],"prompt_cache_key":"shared-cache","client_metadata":{"thread_id":%q,"session_id":"root","turn_id":%q}}`, model, name, topic, turn))}
		opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Metadata: map[string]any{core.ExecutionSessionMetadataKey: downstream, core.CallerScopeMetadataKey: "caller"}}
		ctx := core.WithCodexTransport(core.WithDownstreamWebsocket(t.Context()), &core.CodexTransportState{})
		result, err := e.ExecuteStream(ctx, credential.Clone(), req, opts)
		if overload {
			if err == nil {
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
			if err == nil {
				t.Fatal("overload was not reported")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), name) {
				t.Fatalf("wrong response: %s", output.String())
			}
		}
		e.CloseExecutionSession(downstream)
		return <-frames
	}
	execute("first", "root", "turn-1", "model-a", "downstream-1", false)
	e.RetireExecutionSessions()
	replacement := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	replacement.store = e.store
	e = replacement
	second := execute("second", "root", "turn-1", "model-b", "downstream-2", false)
	if gjson.GetBytes(second, "model").String() != "model-b" {
		t.Fatal("reused OAuth connection did not send the new model")
	}
	if got := gjson.GetBytes(second, "client_metadata.x-codex-turn-state").String(); got != "standard-state" {
		t.Fatalf("metadata priority lost: %q", got)
	}
	third := execute("third", "root", "turn-2", "model-a", "downstream-3", false)
	if got := gjson.GetBytes(third, "client_metadata.x-codex-turn-state").String(); got != "" {
		t.Fatalf("turn state leaked: %q", got)
	}
	execute("overload", "root", "turn-3", "model-a", "downstream-4", true)
	execute("after-overload", "root", "turn-4", "model-a", "downstream-5", false)
	if got := connections.Load(); got != 1 {
		t.Fatalf("same topic opened %d connections", got)
	}
	execute("child", "child", "turn-child", "model-a", "downstream-child", false)
	if got := connections.Load(); got != 2 {
		t.Fatalf("child thread did not get an independent connection: %d", got)
	}
}

func TestCodexTopicCancellationDrainsBeforeNextRequest(t *testing.T) {
	var connections atomic.Int32
	sent := make(chan string, 4)
	finishFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			name := gjson.GetBytes(payload, "input.0.content").String()
			sent <- name
			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, name)))
			if name == "first" {
				<-finishFirst
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[]}}`, name)))
		}
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
	t.Cleanup(func() { e.CloseExecutionSession(auth.CloseAllExecutionSessionsID) })
	credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "key"}}
	options := core.Options{SourceFormat: translator.FormatCodex, Metadata: map[string]any{core.CallerScopeMetadataKey: "caller"}}
	request := func(name, topic string) core.Request {
		return core.Request{Model: "model-a", Payload: []byte(fmt.Sprintf(`{"model":"model-a","input":[{"role":"user","content":%q}],"client_metadata":{"thread_id":%q}}`, name, topic))}
	}
	ctx, cancel := context.WithCancel(core.WithDownstreamWebsocket(t.Context()))
	first, err := e.ExecuteStream(ctx, credential.Clone(), request("first", "root"), options)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "first" {
		t.Fatal(got)
	}
	cancel()
	for range first.Chunks {
	}
	result := make(chan *core.StreamResult, 1)
	errors := make(chan error, 1)
	go func() {
		r, err := e.ExecuteStream(core.WithDownstreamWebsocket(t.Context()), credential.Clone(), request("second", "root"), options)
		result <- r
		errors <- err
	}()
	// A separate child can complete while the parent is still draining.
	child, err := e.ExecuteStream(core.WithDownstreamWebsocket(t.Context()), credential.Clone(), request("child", "child"), options)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range child.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	if got := <-sent; got != "child" {
		t.Fatalf("unfinished parent was reused: %q", got)
	}
	close(finishFirst)
	next := <-result
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for chunk := range next.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		body.Write(chunk.Payload)
	}
	if got := <-sent; got != "second" {
		t.Fatal(got)
	}
	if strings.Contains(body.String(), `"id":"first"`) || !strings.Contains(body.String(), `"id":"second"`) {
		t.Fatalf("response crossed consumers: %s", body.String())
	}
	if connections.Load() != 2 {
		t.Fatalf("connections=%d", connections.Load())
	}
}

func TestCodexTopicReconnectsOnlyAfterPhysicalDisconnect(t *testing.T) {
	var connections atomic.Int32
	peers := make(chan *websocket.Conn, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = peer.Close() }()
		peers <- peer
		for {
			if _, _, err := peer.ReadMessage(); err != nil {
				return
			}
			_ = peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"result","output":[]}}`))
		}
	}))
	defer upstream.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	e.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
	t.Cleanup(func() { e.CloseExecutionSession(auth.CloseAllExecutionSessionsID) })
	credential := &auth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "key", "base_url": upstream.URL}}
	req := core.Request{Model: "model", Payload: []byte(`{"model":"model","input":[],"client_metadata":{"thread_id":"topic"}}`)}
	opts := core.Options{SourceFormat: translator.FormatCodex}
	run := func() {
		t.Helper()
		result, err := e.ExecuteStream(core.WithDownstreamWebsocket(t.Context()), credential.Clone(), req, opts)
		if err != nil {
			t.Fatal(err)
		}
		for c := range result.Chunks {
			if c.Err != nil {
				t.Fatal(c.Err)
			}
		}
	}
	run()
	e.store.mu.Lock()
	var sess *codexWebsocketSession
	for _, candidate := range e.store.sessions {
		sess = candidate
	}
	e.store.mu.Unlock()
	if sess == nil {
		t.Fatal("topic missing")
	}
	peer := <-peers
	_ = peer.Close()
	<-sess.upstreamDisconnectCh
	// A closed upstream does not trigger a background dial.
	if connections.Load() != 1 {
		t.Fatal("background reconnect")
	}
	run()
	if connections.Load() != 2 {
		t.Fatalf("next request did not reconnect: %d", connections.Load())
	}
}

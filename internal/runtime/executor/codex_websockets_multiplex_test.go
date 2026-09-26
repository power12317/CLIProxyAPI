package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type credentialSocketFixture struct {
	e       *CodexWebsocketsExecutor
	auth    *cliproxyauth.Auth
	opened  atomic.Int32
	live    atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
	sockets []*websocket.Conn
}

func newCredentialSocketFixture(t *testing.T, serve func(*websocket.Conn)) *credentialSocketFixture {
	t.Helper()
	f := &credentialSocketFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.sockets = append(f.sockets, conn)
		f.mu.Unlock()
		f.opened.Add(1)
		live := f.live.Add(1)
		for old := f.peak.Load(); live > old && !f.peak.CompareAndSwap(old, live); old = f.peak.Load() {
		}
		defer f.live.Add(-1)
		defer func() { _ = conn.Close() }()
		serve(conn)
	}))
	f.e = NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	f.e.credentialSockets = &helps.CodexWebsocketRegistry{}
	f.e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	f.auth = &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL}}
	t.Cleanup(func() {
		// Only the simulated upstream closes physical sockets during cleanup.
		f.mu.Lock()
		for _, socket := range f.sockets {
			_ = socket.Close()
		}
		f.mu.Unlock()
		server.Close()
		f.e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	})
	return f
}

func credentialRequest(i int) (core.Request, core.Options) {
	model := "gpt-5.4"
	if i%2 == 1 {
		model = "gpt-5.5"
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"request-%02d"}],"prompt_cache_key":"conversation-%d","client_metadata":{"turn_id":"turn-%d"}}`, model, i, i, i))
	return core.Request{Model: model, Payload: body}, core.Options{
		SourceFormat: translator.FormatOpenAIResponse,
		Metadata:     map[string]any{core.ExecutionSessionMetadataKey: fmt.Sprintf("session-%d", i), core.CallerScopeMetadataKey: fmt.Sprintf("caller-%d", i)},
	}
}

func writeLaneEvent(t *testing.T, conn *websocket.Conn, frame, payload []byte) bool {
	t.Helper()
	streamID := gjson.GetBytes(frame, "stream_id").String()
	if streamID == "" {
		t.Error("missing upstream stream_id")
		return false
	}
	payload, _ = sjson.SetBytes(payload, "stream_id", streamID)
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Error(err)
		return false
	}
	return true
}

func completedLaneEvent(marker string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, "resp-"+marker, "msg-"+marker, marker))
}

func collectCredentialStream(ctx context.Context, e *CodexWebsocketsExecutor, auth *cliproxyauth.Auth, req core.Request, opts core.Options) (string, error) {
	ctx = core.WithCodexTransport(ctx, &core.CodexTransportState{})
	result, err := e.ExecuteStream(ctx, auth.Clone(), req, opts)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return out.String(), chunk.Err
		}
		out.Write(chunk.Payload)
	}
	return out.String(), ctx.Err()
}

func TestCredentialWebsocketTenConcurrentConversations(t *testing.T) {
	const count = 10
	frames := make(chan []byte, count)
	allReceived, resumeResponses := make(chan struct{}), make(chan struct{})
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		requests := make([][]byte, 0, count)
		for range count {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests = append(requests, frame)
			frames <- frame
		}
		close(allReceived)
		<-resumeResponses
		// All ten creates must arrive before any response starts. This would
		// deadlock with response-wide serialization even if socket counts passed.
		for _, frame := range requests {
			marker := gjson.GetBytes(frame, "input.0.content").String()
			writeLaneEvent(t, conn, frame, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"output":[]}}`, "resp-"+marker)))
		}
		for i := len(requests) - 1; i >= 0; i-- {
			frame := requests[i]
			marker := gjson.GetBytes(frame, "input.0.content").String()
			writeLaneEvent(t, conn, frame, []byte(fmt.Sprintf(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":%q}}`, "fallback-"+marker)))
			writeLaneEvent(t, conn, frame, []byte(fmt.Sprintf(`{"type":"response.metadata","headers":{"x-codex-turn-state":%q}}`, "preferred-"+marker)))
			writeLaneEvent(t, conn, frame, []byte(fmt.Sprintf(`{"type":"response.output_text.delta","item_id":%q,"output_index":0,"content_index":0,"delta":%q}`, "msg-"+marker, marker)))
		}
		for _, frame := range requests {
			marker := gjson.GetBytes(frame, "input.0.content").String()
			writeLaneEvent(t, conn, frame, completedLaneEvent(marker))
		}
		for range count {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			turn := gjson.GetBytes(frame, "client_metadata.turn_id").String()
			var i int
			_, _ = fmt.Sscanf(turn, "turn-%d", &i)
			want := fmt.Sprintf("preferred-request-%02d", i)
			if got := gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").String(); got != want {
				t.Errorf("turn-state for %s = %q, want %q", turn, got, want)
			}
			writeLaneEvent(t, conn, frame, completedLaneEvent(fmt.Sprintf("next-%02d", i)))
		}
		_, _, _ = conn.ReadMessage()
	})
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			req, opts := credentialRequest(i)
			ctx := t.Context()
			if i%3 == 0 {
				ctx = core.WithDownstreamWebsocket(ctx)
			}
			var out string
			var err error
			if i%3 == 1 {
				result, errExecute := f.e.Execute(core.WithCodexTransport(ctx, &core.CodexTransportState{}), f.auth.Clone(), req, opts)
				out, err = string(result.Payload), errExecute
			} else {
				out, err = collectCredentialStream(ctx, f.e, f.auth, req, opts)
			}
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			if !strings.Contains(out, fmt.Sprintf("request-%02d", i)) || strings.Contains(out, "stream_id") {
				t.Errorf("request %d output: %s", i, out)
			}
			for j := range count {
				if j != i && strings.Contains(out, fmt.Sprintf("request-%02d", j)) {
					t.Errorf("response %d leaked into %d", j, i)
				}
			}
		})
	}
	<-allReceived
	second := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	second.credentialSockets, second.store = f.e.credentialSockets, f.e.store
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(f.e)
	manager.RegisterExecutor(second)
	close(resumeResponses)
	wg.Wait()
	if len(frames) != count || f.opened.Load() != 1 || f.peak.Load() != 1 {
		t.Fatalf("frames=%d sockets=%d peak=%d", len(frames), f.opened.Load(), f.peak.Load())
	}
	// Sequential continuation remains isolated after all interleaved metadata.
	for i := range count {
		req, opts := credentialRequest(i)
		req.Payload, _ = sjson.SetBytes(req.Payload, "input", []any{map[string]any{"role": "user", "content": "next"}})
		if _, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	if f.opened.Load() != 1 {
		t.Fatalf("continuations opened %d sockets", f.opened.Load())
	}
}

func TestCredentialWebsocketCancellationDrainsOnlyItsLane(t *testing.T) {
	firstRead := make(chan struct{})
	allowCompletion := make(chan struct{})
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		close(firstRead)
		_, second, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if gjson.GetBytes(first, "stream_id").String() == gjson.GetBytes(second, "stream_id").String() {
			t.Error("cancelled in-flight lane was recycled before terminal event")
		}
		<-allowCompletion
		writeLaneEvent(t, conn, first, completedLaneEvent("abandoned"))
		writeLaneEvent(t, conn, second, completedLaneEvent("healthy"))
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		req, opts := credentialRequest(0)
		_, err := collectCredentialStream(ctx, f.e, f.auth, req, opts)
		firstDone <- err
	}()
	<-firstRead
	cancel()
	if err := <-firstDone; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	secondDone := make(chan error, 1)
	go func() {
		req, opts := credentialRequest(1)
		out, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts)
		if err == nil && (!strings.Contains(out, "healthy") || strings.Contains(out, "abandoned")) {
			err = fmt.Errorf("incorrect routing: %s", out)
		}
		secondDone <- err
	}()
	close(allowCompletion)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatalf("cancellation closed/replaced socket: opened=%d live=%d", f.opened.Load(), f.live.Load())
	}
}

func TestCredentialWebsocketOverloadAndExecutorReplacementKeepConnection(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		for i := range 3 {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			payload := completedLaneEvent("healthy")
			if i == 0 {
				payload = []byte(`{"type":"error","status":503,"error":{"type":"server_error","code":"server_is_overloaded","message":"overloaded"}}`)
			}
			writeLaneEvent(t, conn, frame, payload)
		}
		_, _, _ = conn.ReadMessage()
	})
	req, opts := credentialRequest(0)
	if _, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts); err == nil {
		t.Fatal("overload was not returned")
	}
	// Session closure releases a logical subscriber, not the credential socket.
	f.e.CloseExecutionSession("session-0")
	second := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	second.credentialSockets, second.store = f.e.credentialSockets, f.e.store
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&CodexAutoExecutor{httpExec: f.e.CodexExecutor, wsExec: f.e})
	manager.RegisterExecutor(&CodexAutoExecutor{httpExec: second.CodexExecutor, wsExec: second})
	changed := f.auth.Clone()
	changed.Attributes["api_key"] = "renewed-token"
	changed.ProxyURL = "http://127.0.0.1:1"
	for i := 1; i < 3; i++ {
		req, opts := credentialRequest(i)
		if _, err := collectCredentialStream(t.Context(), second, changed, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatalf("socket replaced after overload/reload: opened=%d live=%d", f.opened.Load(), f.live.Load())
	}
}

func TestCredentialWebsocketMissingStreamIDFailsWithoutClosingOrRedial(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"unscoped"}}`))
		_, _, _ = conn.ReadMessage()
	})
	for i := range 2 {
		req, opts := credentialRequest(i)
		_, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts)
		if err == nil || !strings.Contains(err.Error(), "multiplexing unavailable") {
			t.Fatalf("ambiguous event did not fail explicitly: %v", err)
		}
	}
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatalf("protocol mismatch closed/replaced socket: opened=%d live=%d", f.opened.Load(), f.live.Load())
	}
}

func TestCredentialWebsocketPeerCloseRebuildsOnlyOnNextRequest(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		_, frame, err := conn.ReadMessage()
		if err == nil {
			writeLaneEvent(t, conn, frame, completedLaneEvent("finished"))
		}
	})
	for i := range 2 {
		req, opts := credentialRequest(i)
		if _, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts); err != nil {
			t.Fatal(err)
		}
		sess := f.e.getOrCreateSession(fmt.Sprintf("session-%d", i))
		sess.connMu.Lock()
		lane := sess.credentialLane
		sess.connMu.Unlock()
		<-lane.ConnectionDone()
		if f.opened.Load() != int32(i+1) {
			t.Fatal("connection rebuilt without a request")
		}
	}
	if f.peak.Load() != 1 {
		t.Fatalf("peak live sockets = %d", f.peak.Load())
	}
}

func TestCredentialWebsocketLifecycleReleaseDoesNotClosePhysicalConnection(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			writeLaneEvent(t, conn, frame, completedLaneEvent("healthy"))
		}
	})
	first, second := &trackedWebsocketLifecycle{}, &trackedWebsocketLifecycle{}
	req, opts := credentialRequest(0)
	for _, lifecycle := range []*trackedWebsocketLifecycle{first, second} {
		opts.ExecutionLifecycle = lifecycle
		if _, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	if first.ends.Load() != 1 {
		t.Fatal("replaced Home selection was not released")
	}
	second.End("registry_drained")
	if second.ends.Load() != 1 {
		t.Fatal("Home selection did not end")
	}
	opts.ExecutionLifecycle = nil
	if _, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts); err != nil {
		t.Fatal(err)
	}
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatal("Home resource release closed the physical connection")
	}
}

func TestCredentialWebsocketTransportFailureDoesNotReplay(t *testing.T) {
	var requests atomic.Int32
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err == nil {
			requests.Add(1)
		}
		// The server may have started tools: no terminal response proves safety.
	})
	req, opts := credentialRequest(0)
	_, err := collectCredentialStream(t.Context(), f.e, f.auth, req, opts)
	if !core.IsCodexReplayUnsafe(err) || requests.Load() != 1 || f.opened.Load() != 1 {
		t.Fatalf("ambiguous request replayed: requests=%d connections=%d err=%v", requests.Load(), f.opened.Load(), err)
	}
}

func TestCredentialWebsocketChatSSEPreservesDownstreamFormat(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return
		}
		writeLaneEvent(t, conn, frame, []byte(`{"type":"response.created","response":{"id":"chat","created_at":1,"output":[]}}`))
		writeLaneEvent(t, conn, frame, []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg","delta":"hello-chat"}`))
		writeLaneEvent(t, conn, frame, completedLaneEvent("hello-chat"))
		_, _, _ = conn.ReadMessage()
	})
	req := core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)}
	output, err := collectCredentialStream(t.Context(), f.e, f.auth, req, core.Options{SourceFormat: translator.FormatOpenAI})
	if err != nil || !strings.Contains(output, "chat.completion.chunk") || !strings.Contains(output, "hello-chat") || strings.Contains(output, "stream_id") {
		t.Fatalf("incorrect Chat SSE: %s %v", output, err)
	}
	f.e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatal("ephemeral downstream request closed the credential socket")
	}
}

func TestCredentialWebsocketDuplexOverloadAndConcurrentHTTP(t *testing.T) {
	f := newCredentialSocketFixture(t, func(conn *websocket.Conn) {
		_, initial, err := conn.ReadMessage()
		if err != nil {
			return
		}
		writeLaneEvent(t, conn, initial, []byte(`{"type":"error","status":503,"error":{"code":"server_is_overloaded","message":"busy"}}`))
		var native, httpFrame []byte
		for range 2 {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if gjson.GetBytes(frame, "stream_id").String() == gjson.GetBytes(initial, "stream_id").String() {
				native = frame
			} else {
				httpFrame = frame
			}
		}
		if native == nil || httpFrame == nil {
			t.Error("native continuation and HTTP request did not share distinct lanes")
			return
		}
		writeLaneEvent(t, conn, native, []byte(`{"type":"response.created","response":{"id":"native","output":[]}}`))
		writeLaneEvent(t, conn, httpFrame, completedLaneEvent("http-healthy"))
		_, steer, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if gjson.GetBytes(steer, "type").String() != "response.steer" {
			t.Error("steering frame not forwarded")
			return
		}
		// Private steering can identify its lane by the owned parent response.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.steer.accepted","steer":{"id":"s","previous_response_id":"native"}}`))
		writeLaneEvent(t, conn, native, []byte(`{"type":"response.completed","response":{"id":"native","output":[]}}`))
		writeLaneEvent(t, conn, native, []byte(`{"type":"response.created","response":{"id":"native-next","previous_response_id":"native","output":[]}}`))
		writeLaneEvent(t, conn, native, []byte(`{"type":"response.completed","response":{"id":"native-next","output":[]}}`))
		_, _, _ = conn.ReadMessage()
	})
	f.e.cfg.Codex.ResponseSteering = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := make(chan core.WebsocketInput, 4)
	ctx = core.WithCodexTransport(core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input), &core.CodexTransportState{})
	req, opts := credentialRequest(0)
	result, err := f.e.ExecuteStream(ctx, f.auth.Clone(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	httpDone := make(chan error, 1)
	completed := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		kind := gjson.GetBytes(chunk.Payload, "type").String()
		id := gjson.GetBytes(chunk.Payload, "response.id").String()
		if kind == "error" {
			input <- core.WebsocketInput{Payload: []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)}
			go func() {
				reqHTTP, optsHTTP := credentialRequest(1)
				out, errHTTP := collectCredentialStream(t.Context(), f.e, f.auth, reqHTTP, optsHTTP)
				if errHTTP == nil && !strings.Contains(out, "http-healthy") {
					errHTTP = fmt.Errorf("missing HTTP response: %s", out)
				}
				httpDone <- errHTTP
			}()
		}
		if kind == "response.created" && id == "native" {
			input <- core.WebsocketInput{Payload: []byte(`{"type":"response.steer","previous_response_id":"native","input":"continue"}`)}
		}
		if kind == "response.completed" && id == "native-next" {
			completed = true
			cancel()
		}
	}
	if !completed {
		t.Fatal("native downstream ended before steering successor")
	}
	if err := <-httpDone; err != nil {
		t.Fatal(err)
	}
	if f.opened.Load() != 1 || f.live.Load() != 1 {
		t.Fatal("duplex cancellation or overload closed the physical connection")
	}
}

package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type basispointsIdleCloseHook struct {
	closed chan struct{}
	once   sync.Once
}

func (*basispointsIdleCloseHook) Levels() []log.Level { return []log.Level{log.InfoLevel} }
func (h *basispointsIdleCloseHook) Fire(entry *log.Entry) error {
	if entry.Message == "basispoints websockets: upstream disconnected" && entry.Data["session_id"] == "6d5ecafe" {
		h.once.Do(func() { close(h.closed) })
	}
	return nil
}

func TestBasispointsWebsocketReusesSessionAcrossTurnsModelsAndReload(t *testing.T) {
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldLevel := logger.GetLevel()
	hook := &basispointsIdleCloseHook{closed: make(chan struct{})}
	logger.AddHook(hook)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
	var connections atomic.Int32
	frames := make(chan []byte, 8)
	pongs := make(chan struct{}, 8)
	closeFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		if connections.Add(1) == 1 {
			go func() { <-closeFirst; _ = conn.Close() }()
		}
		conn.SetPongHandler(func(string) error { pongs <- struct{}{}; return nil })
		for {
			_, raw, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			frames <- raw
			if gjson.GetBytes(raw, "type").String() != "response.create" {
				t.Errorf("new turn did not send response.create: %s", raw)
			}
			response := fmt.Sprintf(`{"type":"response.completed","basispoints_replay_cursor":0,"response":{"id":%q,"model":%q,"status":"completed","output":[]}}`, gjson.GetBytes(raw, "metadata.turn_id").String(), gjson.GetBytes(raw, "model").String())
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(response)); errWrite != nil {
				return
			}
			if errPing := conn.WriteControl(websocket.PingMessage, []byte("idle"), time.Time{}); errPing != nil {
				return
			}
		}
	}))
	defer server.Close()
	// Ensure the server-side disconnect goroutine also exits after a failed assertion.
	var closeOnce sync.Once
	closePeer := func() { closeOnce.Do(func() { close(closeFirst) }) }
	defer closePeer()
	store := helps.NewBasispointsWebsocketSessions()
	t.Cleanup(store.CloseAll)
	newExecutor := func() *BasispointsExecutor {
		e := NewBasispointsExecutor(forceWebsocketTestConfig())
		e.websocketSessions, e.websocketDial = store, basispointsTestDial(server.URL)
		return e
	}
	exec := newExecutor()
	auth := basispointsTestAuth()
	auth.ID = t.Name()
	const session = "6d5ecafe-1111-2222-3333-444444444444"
	var firstRequestID, firstResumeToken, firstTurn string
	for i, model := range []string{"gpt-6-astra", "gpt-6-astra", "gpt-6-luna", "gpt-6-astra"} {
		if i == 2 {
			exec = newExecutor()
		}
		if i == 3 {
			closePeer()
			<-hook.closed
			if connections.Load() != 1 {
				t.Fatal("idle disconnect started an unsolicited reconnect")
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		req := core.Request{Model: model, Payload: []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":%q}],"client_metadata":{"session_id":%q,"turn_id":%q}}`, model, fmt.Sprint(i), session, fmt.Sprintf("turn-%d", i)))}
		opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Metadata: map[string]any{core.CallerScopeMetadataKey: t.Name()}}
		if i%2 == 0 {
			_, err := exec.Execute(ctx, auth, req, opts)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
		} else {
			result, err := exec.ExecuteStream(core.WithDownstreamWebsocket(ctx), auth, req, opts)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					cancel()
					t.Fatal(chunk.Err)
				}
			}
		}
		cancel()
		<-pongs
		frame := <-frames
		requestID := gjson.GetBytes(frame, "basispoints_request_id").String()
		resumeToken := gjson.GetBytes(frame, "basispoints_resume_token").String()
		turn := gjson.GetBytes(frame, "metadata.turn_id").String()
		if i == 0 {
			firstRequestID, firstResumeToken, firstTurn = requestID, resumeToken, turn
		}
		if i > 0 && (requestID == firstRequestID || resumeToken == firstResumeToken || turn == firstTurn) {
			t.Fatal("connection reuse reused per-request protocol state")
		}
		want := int32(1)
		if i == 3 {
			want = 2
		}
		if got := connections.Load(); got != want {
			t.Fatalf("request %d opened %d connections; want %d", i, got, want)
		}
	}
}

func TestBasispointsWebsocketConcurrentRequestsAndAccountRotation(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		connections.Add(1)
		for {
			_, frame, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			id := gjson.GetBytes(frame, "input.1.content.0.text").String()
			response := fmt.Sprintf(`{"type":"response.completed","basispoints_replay_cursor":0,"response":{"id":%q,"status":"completed","output":[]}}`, r.Header.Get("ChatGPT-Account-ID")+"/"+id)
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(response)); errWrite != nil {
				return
			}
		}
	}))
	defer server.Close()
	exec := NewBasispointsExecutor(forceWebsocketTestConfig())
	isolateBasispointsWebsockets(t, exec)
	exec.websocketDial = basispointsTestDial(server.URL)
	a := basispointsTestAuth()
	a.ID, a.Metadata["account_id"] = "selected-account-a", "account-a"
	b := basispointsTestAuth()
	b.ID, b.Metadata["account_id"] = "selected-account-b", "account-b"
	invoke := func(auth *coreauth.Auth, i int) error {
		auth = auth.Clone()
		req := core.Request{Model: "gpt-6-astra", Payload: []byte(fmt.Sprintf(`{"input":[{"role":"user","content":%q}],"client_metadata":{"session_id":"same-session","turn_id":%q}}`, fmt.Sprint(i), fmt.Sprint(i)))}
		result, err := exec.Execute(t.Context(), auth, req, core.Options{SourceFormat: translator.FormatOpenAIResponse})
		if err != nil {
			return err
		}
		if got := gjson.GetBytes(result.Payload, "id").String(); got != fmt.Sprint(auth.Metadata["account_id"])+"/"+fmt.Sprint(i) {
			return fmt.Errorf("request %d received another response: %s", i, got)
		}
		return nil
	}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- invoke(a, i) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("same-session parallel calls opened %d connections", connections.Load())
	}
	if err := invoke(b, 8); err != nil {
		t.Fatal(err)
	}
	if err := invoke(a, 9); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 2 {
		t.Fatal("selected-account rotation did not retain each account's existing socket")
	}
}

func TestBasispointsWebsocketSharedStoreSurvivesExecutorReplacement(t *testing.T) {
	first := NewBasispointsExecutor(forceWebsocketTestConfig())
	second := NewBasispointsExecutor(forceWebsocketTestConfig())
	if first.websocketSessions != second.websocketSessions || first.websocketSessions != helps.SharedBasispointsWebsocketSessions {
		t.Fatal("configuration reload would discard live Basispoints sessions")
	}
}

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type forceWebsocketRoundTripper func(*http.Request) (*http.Response, error)

func (f forceWebsocketRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func forceWebsocketTestConfig() *config.Config {
	return &config.Config{Codex: config.CodexConfig{ForceWebsocket: true}, SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
}

func TestForceWebsocketFiveFailuresThenHTTPAndNextRequestTriesAgain(t *testing.T) {
	var handshakes, httpRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	e := NewCodexAutoExecutor(forceWebsocketTestConfig())
	// Keep the two real transports local: WS sees a rejecting CONNECT proxy,
	// and the HTTP fallback uses the supported request-scoped round tripper.
	wsConfig := forceWebsocketTestConfig()
	wsConfig.ProxyURL = proxy.URL
	e.wsExec = NewCodexWebsocketsExecutor(wsConfig)
	t.Cleanup(func() { e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		attempt := httpRequests.Add(1)
		if got := handshakes.Load(); got != attempt*5 {
			t.Errorf("HTTP request %d after %d WS failures", attempt, got)
		}
		body := `data: {"type":"response.completed","response":{"id":"http-result","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})))
	auth := &cliproxyauth.Auth{ID: "force-fallback", Provider: "codex", Attributes: map[string]string{"api_key": "test"}}
	for range 2 {
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
		result, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":[],"stream":true}`)}, opts)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	}
	if handshakes.Load() != 10 || httpRequests.Load() != 2 {
		t.Fatalf("WS=%d HTTP=%d", handshakes.Load(), httpRequests.Load())
	}
}

func TestForceWebsocketSequenceReuseTurnStateAndContinuation(t *testing.T) {
	var connections atomic.Int32
	frames := make(chan []byte, 3)
	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		headers <- r.Header.Clone()
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{"X-Codex-Turn-State": {"handshake-only"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for i := 0; i < 3; i++ {
			_, body, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			frames <- body
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"event-state"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"later-state"}}`))
			response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`, i)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(response))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	t.Cleanup(func() { e.CloseExecutionSession(t.Name()) })
	for i := 0; i < 3; i++ {
		turn := "turn-1"
		if i == 2 {
			turn = "turn-2"
		}
		input := `[{"role":"user","content":"first"}]`
		if i > 0 {
			input = `[{"role":"user","content":"first"},{"role":"user","content":"next"}]`
		}
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":%s,"stream":true,"stream_options":{"reasoning_summary_delivery":"auto"},"client_metadata":{"turn_id":%q}}`, input, turn))
		if i == 0 {
			body, _ = sjson.SetBytes(body, "generate", false)
		}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}, Headers: http.Header{"User-Agent": {"codex_cli_rs/0.156.1"}}}
		ctx := cliproxyexecutor.WithCodexTransport(t.Context(), &cliproxyexecutor.CodexTransportState{})
		result, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}, opts)
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
		if !strings.Contains(output.String(), "response.completed") {
			t.Fatalf("missing SSE completion: %s", output.String())
		}
		frame := <-frames
		if i == 0 && (!gjson.GetBytes(frame, "generate").Exists() || gjson.GetBytes(frame, "generate").Bool()) {
			t.Fatalf("warmup became generation: %s", frame)
		}
		if i > 0 && gjson.GetBytes(frame, "generate").Exists() {
			t.Fatalf("warmup flag leaked to generation: %s", frame)
		}
		if gjson.GetBytes(frame, "type").String() != "response.create" || !gjson.GetBytes(frame, "stream").Bool() {
			t.Fatalf("non-native frame: %s", frame)
		}
		if gjson.GetBytes(frame, "stream_options.reasoning_summary_delivery").String() != "auto" {
			t.Fatalf("lost WS stream_options: %s", frame)
		}
		state := gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").String()
		if i == 1 {
			if state != "event-state" || gjson.GetBytes(frame, "previous_response_id").String() != "r0" || len(gjson.GetBytes(frame, "input").Array()) != 1 {
				t.Fatalf("bad continuation: %s", frame)
			}
		} else if state != "" {
			t.Fatalf("state leaked across turns or from handshake: %s", frame)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connections=%d, want one reusable socket", connections.Load())
	}
	hs := <-headers
	if hs.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" || hs.Get("Authorization") != "Bearer test" || hs.Get("User-Agent") != "codex_cli_rs/0.156.1" {
		t.Fatalf("unexpected handshake identity: %v", hs)
	}
}

func TestForceWebsocketPostSendDisconnectCannotReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
		requests.Add(1)
		_ = conn.Close()
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	ctx := cliproxyexecutor.WithCodexTransport(t.Context(), &cliproxyexecutor.CodexTransportState{})
	result, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err == nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if !cliproxyexecutor.IsCodexReplayUnsafe(err) || requests.Load() != 1 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
	var scoped cliproxyexecutor.RequestScopedError
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
		t.Fatal("outer retries were not stopped")
	}
}

func TestForceWebsocketPolicyScope(t *testing.T) {
	e := NewCodexAutoExecutor(forceWebsocketTestConfig())
	for _, tc := range []struct {
		provider, url string
		want          bool
	}{
		{"codex", "", true}, {"codex", "https://chatgpt.com/backend-api/codex/", true},
		{"codex", "https://api.openai.com/v1", false}, {"codex", "https://chatgpt.com.example/backend-api/codex", false},
		{"codex", "http://chatgpt.com/backend-api/codex", false}, {"xai", "", false},
	} {
		auth := &cliproxyauth.Auth{Provider: tc.provider, Attributes: map[string]string{"base_url": tc.url}}
		if e.forceWebsocket(auth) != tc.want {
			t.Errorf("unexpected policy for %s %s", tc.provider, tc.url)
		}
	}
}

func TestForceWebsocketIdleDisconnectRebuildsOnlyOnNextRequest(t *testing.T) {
	var connections atomic.Int32
	frames := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, body, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Error(errRead)
			return
		}
		frames <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"closed-response","output":[]}}`))
	}))
	defer server.Close()
	e := NewCodexWebsocketsExecutor(forceWebsocketTestConfig())
	t.Cleanup(func() { e.CloseExecutionSession(t.Name()) })
	disconnected := e.UpstreamDisconnectChan(t.Name())
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	for i := 0; i < 2; i++ {
		ctx := cliproxyexecutor.WithCodexTransport(t.Context(), &cliproxyexecutor.CodexTransportState{})
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}}
		_, err := e.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":[]}`)}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(<-frames, "previous_response_id").Exists() {
			t.Fatal("new connection used an old response ID")
		}
		if i == 0 {
			<-disconnected
			if connections.Load() != 1 {
				t.Fatal("upstream reconnected without a request")
			}
		}
	}
	if connections.Load() != 2 {
		t.Fatalf("connections=%d", connections.Load())
	}
}

func TestForceWebsocketWarmupFallbackDoesNotGenerateOverHTTP(t *testing.T) {
	e := NewCodexAutoExecutor(forceWebsocketTestConfig())
	t.Cleanup(func() { e.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
	state := &cliproxyexecutor.CodexTransportState{}
	state.Failures.Store(5)
	state.HTTPFallback.Store(true)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(t.Context())
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("warmup issued an HTTP generation")
		return nil, errors.New("unexpected generation")
	})))
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test"}}
	result, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","generate":false,"input":[]}`)}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:     map[string]any{cliproxyexecutor.CodexTransportMetadataKey: state},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		events = append(events, gjson.GetBytes(chunk.Payload, "type").String())
	}
	if strings.Join(events, ",") != "response.created,response.completed" {
		t.Fatalf("warmup events: %v", events)
	}
}

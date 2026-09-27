package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const runtimeTerminal = `{"type":"response.completed","response":{"id":"resp_test","object":"response","created_at":1234,"model":"runtime-model","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"key\":\"value\"}","status":"completed","extension":true}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"future_field":{"preserve":true}}}`

type runtimeTestRPC struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func runtimeTestWorker(t *testing.T, id string, handle func(*websocket.Conn, runtimeTestRPC, bridge.Request)) (config.CodexRuntimeWorker, *coreauth.Auth) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cpa-exec-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		for {
			var m runtimeTestRPC
			if c.ReadJSON(&m) != nil {
				return
			}
			switch m.Method {
			case "initialize":
				_ = c.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{}})
			case "initialized":
			case "cpa/capabilities/read":
				_ = c.WriteJSON(map[string]any{"id": m.ID, "result": bridge.Capabilities{ProtocolVersion: 1, RuntimeVersion: "test", UpstreamRevision: "test-sha", CredentialID: id, AccountID: "account", AuthMode: "chatgpt", ExecutionMode: "inference-only", RawEvents: true, Operations: []string{"responses"}}})
			case "cpa/inference/start":
				var req bridge.Request
				if err := json.Unmarshal(m.Params, &req); err != nil {
					t.Error(err)
					return
				}
				handle(c, m, req)
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
	return config.CodexRuntimeWorker{ID: id, Socket: socket, AccountID: "account", Models: []string{"runtime-model"}}, &coreauth.Auth{ID: "codex-runtime:" + id, Provider: coreauth.CodexRuntimeProvider, Attributes: map[string]string{coreauth.AttributeCodexRuntimeID: id, coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "account_id": "account", "socket": socket}}
}

func runtimeTestAccept(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
	_ = c.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{"requestId": req.RequestID, "statusCode": 200, "headers": http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"synthetic-secret"}, "X-Request-Id": {"upstream-id"}}}})
}
func runtimeTestFinish(c *websocket.Conn, req bridge.Request, events ...string) {
	for _, event := range events {
		_ = c.WriteJSON(map[string]any{"method": "cpa/inference/event", "params": map[string]any{"requestId": req.RequestID, "event": json.RawMessage(event)}})
	}
	_ = c.WriteJSON(map[string]any{"method": "cpa/inference/completed", "params": map[string]any{"requestId": req.RequestID}})
}
func runtimeTestExecutor(w config.CodexRuntimeWorker) *CodexRuntimeExecutor {
	return NewCodexRuntimeExecutor(&config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, Workers: []config.CodexRuntimeWorker{w}}}})
}
func runtimeTestOptions() coreexecutor.Options {
	return coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: "caller-a", coreexecutor.CanonicalSessionIDMetadataKey: "session"}}
}
func equalRuntimeJSON(t *testing.T, a, b []byte) {
	t.Helper()
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil || !reflect.DeepEqual(left, right) {
		t.Fatalf("JSON differs:\n%s\n%s", a, b)
	}
}

func TestCodexRuntimeNativeRequestAndResponseFidelity(t *testing.T) {
	captured := make(chan bridge.Request, 1)
	w, a := runtimeTestWorker(t, "native", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		captured <- r
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, runtimeTerminal)
	})
	body := []byte(`{"model":"runtime-model","input":[{"role":"user","content":"inspect"}],"instructions":"caller instructions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"parallel_tool_calls":false,"client_metadata":{"extension":"client-value"},"future_field":[1,true],"prompt_cache_key":"untrusted-session","stream":false}`)
	opts := runtimeTestOptions()
	opts.Headers = http.Header{"Authorization": {"synthetic-bearer"}, "Cookie": {"synthetic-cookie"}}
	res, err := runtimeTestExecutor(w).Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: body}, opts)
	if err != nil {
		t.Fatal(err)
	}
	r := <-captured
	for _, field := range []string{"input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "client_metadata", "future_field"} {
		equalRuntimeJSON(t, []byte(gjson.GetBytes(body, field).Raw), []byte(gjson.GetBytes(r.Request, field).Raw))
	}
	if r.CredentialID != w.ID || r.AccountID != w.AccountID || r.SourceFormat != "openai-response" || len(r.SessionID) != 64 || r.SessionID != gjson.GetBytes(r.Request, "prompt_cache_key").String() || !gjson.GetBytes(r.Request, "stream").Bool() {
		t.Fatal("incorrect identity/normalization", r)
	}
	encoded, _ := json.Marshal(r)
	if bytes.Contains(encoded, []byte("synthetic-bearer")) || bytes.Contains(encoded, []byte("synthetic-cookie")) || bytes.Contains(encoded, []byte("caller-a")) {
		t.Fatal("caller secret crossed bridge")
	}
	equalRuntimeJSON(t, []byte(gjson.Get(runtimeTerminal, "response").Raw), res.Payload)
	if res.Headers.Get("Set-Cookie") != "" || res.Headers.Get("X-Request-Id") != "upstream-id" {
		t.Fatal("response headers not filtered")
	}
}

func TestCodexRuntimeStreamPreservesUnknownEventsAndTools(t *testing.T) {
	future := `{"type":"response.future_extension","unknown":{"a":[1,true]},"delta":"unchanged"}`
	w, a := runtimeTestWorker(t, "stream", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, future, runtimeTerminal)
	})
	res, err := runtimeTestExecutor(w).ExecuteStream(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	var events [][]byte
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if !bytes.HasSuffix(chunk.Payload, []byte("\n\n")) {
			t.Fatal("invalid SSE framing")
		}
		events = append(events, bytes.TrimSpace(bytes.TrimPrefix(chunk.Payload, []byte("data: "))))
	}
	if len(events) != 2 {
		t.Fatalf("event count %d", len(events))
	}
	equalRuntimeJSON(t, events[0], []byte(future))
	equalRuntimeJSON(t, events[1], []byte(runtimeTerminal))
}

func TestCodexRuntimeCallerSessionIsolation(t *testing.T) {
	captured := make(chan string, 4)
	w, a := runtimeTestWorker(t, "sessions", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		captured <- r.SessionID
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, runtimeTerminal)
	})
	e := runtimeTestExecutor(w)
	var sessions []string
	for _, scope := range []string{"caller-a", "caller-a", "caller-b", ""} {
		opts := runtimeTestOptions()
		opts.Metadata[coreexecutor.CallerScopeMetadataKey] = scope
		if _, err := e.Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}, opts); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, <-captured)
	}
	if sessions[0] != sessions[1] || sessions[0] == sessions[2] || sessions[0] == sessions[3] {
		t.Fatal("caller session isolation failed", sessions)
	}
}

func TestCodexRuntimeCrossProtocolToolCalls(t *testing.T) {
	for _, tc := range []struct {
		format           sdktranslator.Format
		body, outputPath string
	}{
		{sdktranslator.FormatOpenAI, `{"model":"runtime-model","messages":[{"role":"user","content":"inspect"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, "choices.0.message.tool_calls.0.function.name"},
		{sdktranslator.FormatClaude, `{"model":"runtime-model","messages":[{"role":"user","content":"inspect"}],"max_tokens":100,"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, "content.0.name"},
	} {
		t.Run(tc.format.String(), func(t *testing.T) {
			captured := make(chan bridge.Request, 1)
			w, a := runtimeTestWorker(t, "tools", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
				captured <- r
				runtimeTestAccept(c, m, r)
				runtimeTestFinish(c, r, runtimeTerminal)
			})
			opts := runtimeTestOptions()
			opts.SourceFormat = tc.format
			res, err := runtimeTestExecutor(w).Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(tc.body)}, opts)
			if err != nil {
				t.Fatal(err)
			}
			r := <-captured
			if gjson.GetBytes(r.Request, "tools.0.name").String() != "lookup" || !gjson.GetBytes(r.Request, "input").IsArray() {
				t.Fatalf("input translation lost tools: %s", r.Request)
			}
			if gjson.GetBytes(res.Payload, tc.outputPath).String() != "lookup" {
				t.Fatalf("output translation lost tool call: %s", res.Payload)
			}
		})
	}
}

func TestCodexRuntimeRejectUnsupportedBeforeInference(t *testing.T) {
	w, a := runtimeTestWorker(t, "unsupported", func(*websocket.Conn, runtimeTestRPC, bridge.Request) {
		t.Error("unsupported request reached inference")
	})
	e := runtimeTestExecutor(w)
	for _, body := range []string{`{"previous_response_id":"old"}`, `{"generate":false}`} {
		if _, err := e.Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(body)}, runtimeTestOptions()); err == nil {
			t.Fatal("unsupported request accepted")
		}
	}
	opts := runtimeTestOptions()
	opts.Alt = "responses/compact"
	if _, err := e.Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, opts); err == nil {
		t.Fatal("unsupported compact accepted")
	}
	w.AccountID = "wrong"
	if _, err := runtimeTestExecutor(w).Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions()); err == nil {
		t.Fatal("account mismatch accepted")
	}
}

func TestCodexRuntimeAmbiguousFailureDoesNotReplay(t *testing.T) {
	var calls atomic.Int32
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 0, 5)
	manager.SetConfig(&config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{coreauth.CodexRuntimeProvider: {{Status: 502, Match: []string{"Codex runtime"}, Action: coreauth.RequestScopedActionContinue}}}})
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
	for _, id := range []string{"replay-a", "replay-b"} {
		w, a := runtimeTestWorker(t, id, func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) { calls.Add(1); runtimeTestAccept(c, m, r) })
		cfg.Codex.Runtime.Workers = append(cfg.Codex.Runtime.Workers, w)
		registry.GetGlobalRegistry().RegisterClient(a.ID, coreauth.CodexRuntimeProvider, []*registry.ModelInfo{{ID: "runtime-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.RegisterExecutor(NewCodexRuntimeExecutor(cfg))
	_, err := manager.Execute(context.Background(), []string{coreauth.CodexRuntimeProvider}, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
	if err == nil || calls.Load() != 1 {
		t.Fatalf("ambiguous inference replayed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCodexRuntimeFailedNativeResponseRetainsDetails(t *testing.T) {
	terminal := `{"type":"response.failed","response":{"id":"failed","status":"failed","error":{"code":"model_failure","message":"synthetic failure"},"output":[]}}`
	w, a := runtimeTestWorker(t, "failed", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, terminal)
	})
	res, err := runtimeTestExecutor(w).Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Payload), "synthetic failure") {
		t.Fatal("native failure detail discarded")
	}
}

func TestCodexRuntimeCancellationReleasesBlockedStream(t *testing.T) {
	closed := make(chan struct{})
	w, a := runtimeTestWorker(t, "cancel", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		runtimeTestAccept(c, m, r)
		_ = c.WriteJSON(map[string]any{"method": "cpa/inference/event", "params": map[string]any{"requestId": r.RequestID, "event": json.RawMessage(`{"type":"response.output_text.delta","delta":"part"}`)}})
		_, _, _ = c.ReadMessage()
		close(closed)
	})
	logCtx, ginCtx := runtimeLoggingContext()
	ctx, cancel := context.WithCancel(logCtx)
	defer cancel()
	e := runtimeTestExecutor(w)
	e.cfg.RequestLog = true
	res, err := e.ExecuteStream(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-closed
	for range res.Chunks {
	}
	if responseLog := runtimeLogText(t, ginCtx, "API_RESPONSE"); !strings.Contains(responseLog, "context canceled") {
		t.Fatal("cancellation missing from response log", responseLog)
	}
}

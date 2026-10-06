package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const runtimeTerminal = `{"type":"response.completed","response":{"id":"resp_test","object":"response","created_at":1234,"model":"runtime-model","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"key\":\"value\"}","status":"completed","extension":true}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"future_field":{"preserve":true}}}`

type runtimeTestRPC struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func runtimeTestMaster(t *testing.T, id string, handle func(*websocket.Conn, runtimeTestRPC, bridge.Request), customize ...func(*bridge.Capabilities)) (config.CodexRuntimeConfig, *coreauth.Auth) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("master connection unexpectedly contains authentication")
		}
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
				caps := bridge.Capabilities{ProtocolVersion: 3, RuntimeVersion: "test", UpstreamRevision: "test-sha", ExecutionMode: "inference-only", RawBody: true, UpstreamLogs: true, UpstreamBodyLogs: true, Operations: []string{"responses"}}
				for _, apply := range customize {
					apply(&caps)
				}
				_ = c.WriteJSON(map[string]any{"id": m.ID, "result": caps})
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
	}))
	t.Cleanup(server.Close)
	return config.CodexRuntimeConfig{Enabled: true, URL: "ws" + strings.TrimPrefix(server.URL, "http")}, runtimeTestAuth(id)
}

func runtimeTestAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, FileName: id, Provider: "codex", Metadata: map[string]any{"type": "codex", "codex_cli": map[string]any{"enabled": true}}, Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}}
}

func runtimeTestAccept(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
	_ = c.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{"requestId": req.RequestID, "statusCode": 200, "headers": http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"synthetic-secret"}, "X-Request-Id": {"upstream-id"}}}})
}
func runtimeTestBody(c *websocket.Conn, req bridge.Request, body []byte) {
	_ = c.WriteJSON(map[string]any{"method": "cpa/inference/body", "params": map[string]any{"requestId": req.RequestID, "bodyBase64": body}})
}
func runtimeTestFinish(c *websocket.Conn, req bridge.Request, events ...string) {
	for _, event := range events {
		runtimeTestBody(c, req, []byte("data: "+event+"\n\n"))
	}
	_ = c.WriteJSON(map[string]any{"method": "cpa/inference/completed", "params": map[string]any{"requestId": req.RequestID}})
}
func runtimeTestExecutor(runtime config.CodexRuntimeConfig) *CodexRuntimeExecutor {
	return NewCodexRuntimeExecutor(&config.Config{Codex: config.CodexConfig{Runtime: runtime}})
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
	w, a := runtimeTestMaster(t, "native", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
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
	if r.CredentialID != a.ID || r.SourceFormat != "openai-response" || len(r.SessionID) != 64 || r.SessionID != gjson.GetBytes(r.Request, "prompt_cache_key").String() || !gjson.GetBytes(r.Request, "stream").Bool() {
		t.Fatal("incorrect identity/normalization", r)
	}
	encoded, _ := json.Marshal(r)
	if bytes.Contains(encoded, []byte("synthetic-bearer")) || bytes.Contains(encoded, []byte("synthetic-cookie")) || bytes.Contains(encoded, []byte("caller-a")) {
		t.Fatal("caller secret crossed bridge")
	}
	equalRuntimeJSON(t, helps.EnsureResponsesUsageDetails([]byte(gjson.Get(runtimeTerminal, "response").Raw)), res.Payload)
	if res.Headers.Get("Set-Cookie") != "" || res.Headers.Get("X-Request-Id") != "upstream-id" {
		t.Fatal("response headers not filtered")
	}
}

func TestCodexRuntimeStreamPreservesUnknownEventsAndTools(t *testing.T) {
	future := `{"type":"response.future_extension","unknown":{"a":[1,true]},"delta":"unchanged"}`
	w, a := runtimeTestMaster(t, "stream", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
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
	w, a := runtimeTestMaster(t, "sessions", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
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
			w, a := runtimeTestMaster(t, "tools", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
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
	w, a := runtimeTestMaster(t, "unsupported", func(*websocket.Conn, runtimeTestRPC, bridge.Request) {
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

}

func TestCodexRuntimeAmbiguousFailureDoesNotReplay(t *testing.T) {
	var calls atomic.Int32
	runtime, _ := runtimeTestMaster(t, "Original Account.json", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) { calls.Add(1); runtimeTestAccept(c, m, r) })
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 0, 5)
	manager.SetConfig(&config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{"codex": {{Status: 502, Match: []string{"Codex runtime"}, Action: coreauth.RequestScopedActionContinue}}}})
	dir := t.TempDir()
	for _, id := range []string{"Original Account.json", "sub/Second.JSON"} {
		a := runtimeTestAuth(id)
		path := filepath.Join(dir, filepath.FromSlash(id))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := codexshared.Write(path, a.Metadata); err != nil {
			t.Fatal(err)
		}
		a.Attributes[coreauth.AttributePath] = path
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.RegisterExecutor(NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{Runtime: runtime}}))
	_, err := manager.Execute(context.Background(), []string{"codex"}, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
	if err == nil || calls.Load() != 1 {
		t.Fatalf("ambiguous inference replayed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCodexRuntimeFailedNativeResponseRetainsDetails(t *testing.T) {
	terminal := `{"type":"response.failed","response":{"id":"failed","status":"failed","error":{"code":"model_failure","message":"synthetic failure"},"output":[]}}`
	w, a := runtimeTestMaster(t, "failed", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, terminal)
	})
	_, err := runtimeTestExecutor(w).Execute(context.Background(), a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
	var failure *bridge.Error
	if !errors.As(err, &failure) || !strings.Contains(string(failure.ResponseBody()), "synthetic failure") {
		t.Fatalf("native failure detail discarded: %v", err)
	}
}

func TestCodexRuntimeCancellationReleasesBlockedStream(t *testing.T) {
	closed := make(chan struct{})
	w, a := runtimeTestMaster(t, "cancel", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		runtimeTestAccept(c, m, r)
		runtimeTestBody(c, r, []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"part\"}\n\n"))
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

func TestCodexRuntimeMasterRoutesOriginalCredentialIDsConcurrently(t *testing.T) {
	captured := make(chan bridge.Request, 2)
	cfg, _ := runtimeTestMaster(t, "ignored", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
		captured <- r
		runtimeTestAccept(c, m, r)
		runtimeTestFinish(c, r, runtimeTerminal)
	})
	e := runtimeTestExecutor(cfg)
	errors := make(chan error, 2)
	ids := []string{"Existing Personal Account.JSON", "teams/Original-Credential.json"}
	for _, id := range ids {
		go func() {
			_, err := e.Execute(context.Background(), runtimeTestAuth(id), coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}, runtimeTestOptions())
			errors <- err
		}()
	}
	seen := make(map[string]string)
	for range ids {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		r := <-captured
		seen[r.CredentialID] = r.SessionID
	}
	if len(seen) != 2 || seen[ids[0]] == "" || seen[ids[1]] == "" || seen[ids[0]] == seen[ids[1]] {
		t.Fatalf("master credential identities were remapped: %#v", seen)
	}
}

func TestCodexRuntimeRawBodyChunkingAndMultilineSSE(t *testing.T) {
	wire := ": comment\r\nevent: future\r\nid: id-kept\r\ndata: {\r\ndata: \"type\":\"response.future_extension\",\"delta\":\"你好\"}\r\n\r\ndata: " + runtimeTerminal + "\n\n: final partial line"
	for _, stream := range []bool{true, false} {
		cfg, auth := runtimeTestMaster(t, "Original File.json", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
			runtimeTestAccept(c, m, r)
			for _, b := range []byte(wire) {
				runtimeTestBody(c, r, []byte{b})
			}
			runtimeTestFinish(c, r)
		})
		executor := runtimeTestExecutor(cfg)
		req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}
		if stream {
			result, err := executor.ExecuteStream(context.Background(), auth, req, runtimeTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				out.Write(chunk.Payload)
			}
			if out.String() != wire {
				t.Fatalf("original SSE changed:\n%s", out.String())
			}
		} else {
			result, err := executor.Execute(context.Background(), auth, req, runtimeTestOptions())
			if err != nil || !strings.Contains(string(result.Payload), `"name":"lookup"`) {
				t.Fatalf("nonstream reassembly failed: %s, %v", result.Payload, err)
			}
		}
	}
}

func TestCodexRuntimeEOFWithoutTerminalFailsInCPA(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg, auth := runtimeTestMaster(t, "incomplete.json", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
			runtimeTestAccept(c, m, r)
			runtimeTestFinish(c, r, `{"type":"response.output_text.delta","delta":"unfinished"}`)
		})
		executor := runtimeTestExecutor(cfg)
		req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}
		var err error
		if stream {
			var result *coreexecutor.StreamResult
			result, err = executor.ExecuteStream(context.Background(), auth, req, runtimeTestOptions())
			if err == nil {
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
		} else {
			_, err = executor.Execute(context.Background(), auth, req, runtimeTestOptions())
		}
		var failure *bridge.Error
		if !errors.As(err, &failure) || failure.StatusCode() != 408 || !strings.Contains(err.Error(), "before response.completed") {
			t.Fatalf("normal body EOF hid missing terminal (stream=%v): %v", stream, err)
		}
	}
}

func TestCodexRuntimeStreamToolCallsUseCPAProtocolTranslators(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		t.Run(format.String(), func(t *testing.T) {
			cfg, auth := runtimeTestMaster(t, "tool-stream.json", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
				runtimeTestAccept(c, m, r)
				// The master provides bytes only; CPA extracts and translates function events.
				runtimeTestFinish(c, r,
					`{"type":"response.created","response":{"id":"resp_test","model":"runtime-model","created_at":1234}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"key\":\"value\"}"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"key\":\"value\"}"}}`,
					runtimeTerminal,
				)
			})
			opts := runtimeTestOptions()
			opts.ResponseFormat = format
			result, err := runtimeTestExecutor(cfg).ExecuteStream(context.Background(), auth, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}, opts)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), "lookup") || !strings.Contains(output.String(), "value") {
				t.Fatalf("CPA translation lost tool content: %s", output.String())
			}
		})
	}
}

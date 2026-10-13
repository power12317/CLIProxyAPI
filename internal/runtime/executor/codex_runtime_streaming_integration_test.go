package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Each upstream phase is released only after a real HTTP client receives the
// preceding business event. Waiting for native completion therefore deadlocks
// this test until its watchdog fails, without relying on timing sleeps.
type nativeRuntimeStreamProbe struct {
	events chan []byte
	stop   chan struct{}
	once   sync.Once
}

func newNativeRuntimeStreamProbe() *nativeRuntimeStreamProbe {
	return &nativeRuntimeStreamProbe{events: make(chan []byte), stop: make(chan struct{})}
}

func (p *nativeRuntimeStreamProbe) close() { p.once.Do(func() { close(p.stop) }) }

func (p *nativeRuntimeStreamProbe) serve(ctx context.Context, write func([]byte) error) {
	for {
		select {
		case event := <-p.events:
			if err := write(event); err != nil || gjson.GetBytes(event, "type").String() == "response.completed" {
				return
			}
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func testNativeRuntimeLiveHTTP(t *testing.T, handler http.Handler, probe *nativeRuntimeStreamProbe, home string) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer probe.close()
	// The fixture's default transport rejects CPA model egress. This separate
	// client connects only to CPA's real HTTP listener.
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true,"input":[{"role":"user","content":"live-tool-stream"}],"tools":[{"type":"custom","name":"exec","format":{"type":"text"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	type result struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan result, 1)
	started := time.Now()
	go func() {
		response, errRequest := client.Do(request)
		responseCh <- result{response: response, err: errRequest}
	}()
	send := func(event any) {
		t.Helper()
		data, errMarshal := json.Marshal(event)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		select {
		case probe.events <- data:
		case <-ctx.Done():
			t.Fatal("upstream did not reach next generation phase", ctx.Err())
		}
	}
	send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_live", "status": "in_progress"}})
	var response *http.Response
	select {
	case result := <-responseCh:
		if result.err != nil {
			t.Fatal("client did not receive headers before native completion", result.err)
		}
		response = result.response
	case <-ctx.Done():
		t.Fatal("client did not receive headers while upstream was paused before completion")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("unexpected client status %d: %s", response.StatusCode, body)
	}
	reader := bridge.NewSSEReader(response.Body)
	receive := func(kind string) []byte {
		t.Helper()
		frame, errRead := reader.Next()
		if errRead != nil || gjson.GetBytes(frame.Data, "type").String() != kind {
			t.Fatalf("expected live %s while upstream is paused: %s (%v)", kind, frame.Data, errRead)
		}
		return frame.Data
	}
	receive("response.created")
	t.Logf("first business event received at %s; upstream completion not released", time.Since(started))
	item := map[string]any{"id": "ctc_live", "type": "custom_tool_call", "call_id": "call_live", "name": "exec", "input": "", "status": "in_progress"}
	send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	receive("response.output_item.added")
	parts := []string{"touch ", filepath.Join(home, "must-not-be-created"), " # client tool only"}
	for index, part := range parts {
		send(map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc_live", "delta": part})
		data := receive("response.custom_tool_call_input.delta")
		if gjson.GetBytes(data, "delta").String() != part {
			t.Fatalf("tool argument delta changed: %s", data)
		}
		t.Logf("business delta %d received at %s; next phase and completion not released", index+1, time.Since(started))
	}
	input := strings.Join(parts, "")
	send(map[string]any{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": "ctc_live", "input": input})
	receive("response.custom_tool_call_input.done")
	item["input"], item["status"] = input, "completed"
	send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	receive("response.output_item.done")
	send(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_live", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}}})
	terminal := receive("response.completed")
	if gjson.GetBytes(terminal, "response.output.#").Int() != 1 || gjson.GetBytes(terminal, "response.output.0.call_id").String() != "call_live" {
		t.Fatalf("tool call duplicated or lost: %s", terminal)
	}
	if remaining, errRead := io.ReadAll(response.Body); errRead != nil || strings.TrimSpace(string(remaining)) != "" {
		t.Fatalf("unexpected trailing stream: %s (%v)", remaining, errRead)
	}
	if _, errStat := os.Stat(filepath.Join(home, "must-not-be-created")); !os.IsNotExist(errStat) {
		t.Fatalf("server executed a client tool: %v", errStat)
	}
}

func testNativeRuntimeHTTPCancellation(t *testing.T, handler http.Handler, cancelled <-chan struct{}) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true,"input":[{"role":"user","content":"cancel-response"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("generation did not begin streaming before cancellation", err)
	}
	defer func() { _ = response.Body.Close() }()
	frame, err := bridge.NewSSEReader(response.Body).Next()
	if err != nil || gjson.GetBytes(frame.Data, "type").String() != "response.created" {
		t.Fatalf("generation event missing: %s (%v)", frame.Data, err)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelled:
		t.Log("real HTTP client cancellation reached the native upstream")
	case <-time.After(5 * time.Second):
		t.Fatal("native model request remained active after client cancellation")
	}
}

func testNativeRuntimeHTTPFailure(t *testing.T, handler http.Handler, marker string, stream bool, expectedMessage string) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	payload := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"input":[{"role":"user","content":%q}]}`, stream, marker)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("native failure did not reach the HTTP client", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("error response did not finish", err)
	}
	if stream && strings.HasPrefix(marker, "fail-after-output") {
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "delivered-partial") || !strings.Contains(string(body), `"type":"error"`) || strings.Contains(string(body), `"type":"response.completed"`) {
			t.Fatalf("partial stream did not terminate with the native error: status=%d body=%s", response.StatusCode, body)
		}
	} else if response.StatusCode < 400 || gjson.GetBytes(body, "error.message").String() == "" || strings.Contains(string(body), "delivered-partial") {
		t.Fatalf("native error was replaced with success or partial output: status=%d body=%s", response.StatusCode, body)
	}
	if expectedMessage != "" && !strings.Contains(string(body), expectedMessage) {
		t.Fatalf("native error details lost: expected=%q body=%s", expectedMessage, body)
	}
	t.Logf("native error reached the HTTP client: stream=%t status=%d", stream, response.StatusCode)
}

func TestCodexRuntimeCPARetryDoesNotWaitForDiagnostics(t *testing.T) {
	for _, sameWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_worker=%t", sameWorker), func(t *testing.T) {
			testCodexRuntimeCPARetryDoesNotWaitForDiagnostics(t, sameWorker)
		})
	}
}

func testCodexRuntimeCPARetryDoesNotWaitForDiagnostics(t *testing.T, sameWorker bool) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls, directCalls atomic.Int32
	requestIDs := make(chan string, 2)
	settings, _ := runtimeTestMaster(t, "unused", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
		call := calls.Add(1)
		requestIDs <- req.RequestID
		runtimeLogNotification(conn, req, runtimeLogRequest())
		if call == 1 {
			runtimeTestNativeError(conn, rpc, http.StatusServiceUnavailable, `{"error":{"message":"retry-through-CPA"}}`)
			<-release
			runtimeLogNotification(conn, req, bridge.UpstreamLog{Kind: "response", StatusCode: 503})
			runtimeLogNotification(conn, req, bridge.UpstreamLog{Kind: "body", BodyBytes: []byte("first-attempt-diagnostics")})
			_ = conn.WriteJSON(map[string]any{"method": "cpa/inference/diagnostics/completed", "params": map[string]any{"requestId": req.RequestID}})
			return
		}
		expectedID := "c-runtime.json"
		if sameWorker {
			expectedID = "a-runtime.json"
		}
		if req.CredentialID != expectedID {
			t.Errorf("retry selected unexpected credential %q", req.CredentialID)
		}
		runtimeLogNotification(conn, req, bridge.UpstreamLog{Kind: "response", StatusCode: 200})
		runtimeTestAccept(conn, rpc, req)
		runtimeTestFinish(conn, req, `{"type":"response.created","response":{"id":"resp_test"}}`, runtimeTerminal)
	})
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}, AuthDir: t.TempDir()}
	cfg.RequestLog = true
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	if sameWorker {
		manager.SetRetryConfig(1, 0, 0)
	}
	manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
	localUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer localUpstream.Close()
	ids := []string{"a-runtime.json", "b-local.json", "c-runtime.json"}
	if sameWorker {
		ids = ids[:2]
	}
	for _, id := range ids {
		auth := runtimeTestAuth(id)
		auth.Metadata["disable_cooling"] = true
		auth.Metadata["access_token"] = "synthetic-token"
		if id == "b-local.json" {
			auth.Metadata["codex_cli"] = map[string]any{"enabled": false}
			auth.Attributes["base_url"] = localUpstream.URL
		}
		auth.Attributes[coreauth.AttributePath] = filepath.Join(cfg.AuthDir, id)
		if err := codexshared.Write(auth.Attributes[coreauth.AttributePath], auth.Metadata); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	router := gin.New()
	logs := make(chan string, 1)
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), "same-cpa-source-request"))
		c.Next()
		if value, ok := c.Get("API_RESPONSE"); ok {
			logs <- string(value.([]byte))
		} else {
			logs <- ""
		}
	})
	handler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(router)
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"runtime-model","stream":true,"input":[]}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("CPA retry waited for previous diagnostic completion", err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bridge.NewSSEReader(response.Body)
	for _, kind := range []string{"response.created", "response.completed"} {
		frame, errRead := reader.Next()
		if errRead != nil || gjson.GetBytes(frame.Data, "type").String() != kind {
			t.Fatalf("CPA retry did not deliver %s while diagnostics were held: %s (%v)", kind, frame.Data, errRead)
		}
	}
	if calls.Load() != 2 || directCalls.Load() != 0 {
		t.Fatalf("retry ownership changed: CPA to master=%d CPA to model=%d", calls.Load(), directCalls.Load())
	}
	if first, second := <-requestIDs, <-requestIDs; first != second || first != "same-cpa-source-request" {
		t.Fatalf("CPA retry lost source request identity: first=%s second=%s", first, second)
	}
	unblock()
	_, _ = io.ReadAll(response.Body)
	select {
	case log := <-logs:
		first := strings.Index(log, "first-attempt-diagnostics")
		second := strings.Index(log, "=== API RESPONSE 2 ===")
		if first < 0 || second <= first || !strings.Contains(log[second:], "response.completed") || strings.Contains(log[second:], "retry-through-CPA") {
			t.Fatalf("retry diagnostics mixed or lost: %s", log)
		}
	case <-ctx.Done():
		t.Fatal("diagnostic cleanup did not finish", ctx.Err())
	}
}

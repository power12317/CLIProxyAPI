package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This exercises the real master and child worker with fake credentials and a local upstream.
func TestCodexRuntimeImagesRealWorker(t *testing.T) {
	testCodexNativeRuntimeRealWorker(t, "http")
}

func TestCodexRuntimeWebsocketRealWorker(t *testing.T) {
	testCodexNativeRuntimeRealWorker(t, "websocket")
}

func TestCodexRuntimeHandshakeFailureRealWorker(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUpgradeRequired} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			testCodexNativeRuntimeRealWorker(t, fmt.Sprintf("handshake-%d", status))
		})
	}
}

func testCodexNativeRuntimeRealWorker(t *testing.T, nativeTransport string) {
	binary := os.Getenv("CODEX_RUNTIME_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_TEST_BINARY to the native-model-runtime codex-app-server")
	}
	home := t.TempDir()
	type exchange struct {
		path      string
		body      []byte
		headers   http.Header
		websocket bool
	}
	requests := make(chan exchange, 32)
	cancelled := make(chan struct{}, 4)
	var workerCalls, cpaCalls, websocketAttempts atomic.Int32
	var cpaRetryCalls atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
		cpaCalls.Add(1)
		return nil, fmt.Errorf("CPA direct upstream request forbidden")
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	liveStream := newNativeRuntimeStreamProbe()
	t.Cleanup(liveStream.close)
	responseEvents := func(body []byte) ([][]byte, bool) {
		events := nativeRuntimeResponseEvents(t, home, body)
		if bytes.Contains(body, []byte("routing-ticket-probe")) {
			metadata, _ := json.Marshal(map[string]any{"type": "codex.response.metadata", "headers": map[string]string{"x-codex-turn-state": strings.Repeat("T", 780)}})
			events = append([][]byte{metadata}, events...)
		}
		if bytes.Contains(body, []byte("fail-after-output")) {
			events = events[:len(events)-1]
			for i, event := range events {
				event = bytes.ReplaceAll(event, []byte("native result"), []byte("delivered-partial"))
				event = bytes.ReplaceAll(event, []byte("msg_native"), []byte("msg_partial"))
				events[i] = bytes.ReplaceAll(event, []byte("resp_native"), []byte("resp_partial"))
			}
			return events, true
		}
		return events, false
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/v1/images/") && r.URL.Path != "/v1/responses" {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			websocketAttempts.Add(1)
			if strings.HasPrefix(nativeTransport, "handshake-") {
				status, _ := strconv.Atoi(strings.TrimPrefix(nativeTransport, "handshake-"))
				http.Error(w, "controlled native handshake failure", status)
				return
			}
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, http.Header{"X-Request-Id": {"native-ws-trace"}})
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			if r.Header.Get("Authorization") != "Bearer synthetic-worker-token" {
				t.Error("native WebSocket did not use the worker credential")
			}
			for {
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				workerCalls.Add(1)
				requests <- exchange{path: r.URL.Path, body: body, headers: r.Header.Clone(), websocket: true}
				if bytes.Contains(body, []byte("fail-before-output")) || bytes.Contains(body, []byte("CPA-owned-retry")) && cpaRetryCalls.Add(1) == 1 {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":503,"error":{"message":"controlled native pre-output failure","type":"server_error","code":"server_error"}}`))
					return
				}
				if bytes.Contains(body, []byte("live-tool-stream")) {
					liveStream.serve(r.Context(), func(event []byte) error {
						return conn.WriteMessage(websocket.TextMessage, event)
					})
					continue
				}
				if bytes.Contains(body, []byte("cancel-response")) {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_cancel"}}`))
					_, next, errRead := conn.ReadMessage()
					if errRead == nil && gjson.GetBytes(next, "type").String() != "response.cancel" {
						t.Error("native request was replayed after cancellation")
					}
					cancelled <- struct{}{}
					return
				}
				events, disconnect := responseEvents(body)
				for _, event := range events {
					if err := conn.WriteMessage(websocket.TextMessage, event); err != nil {
						return
					}
				}
				if disconnect {
					return
				}
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Content-Encoding") == "zstd" {
			decoder, errDecoder := zstd.NewReader(nil)
			if errDecoder != nil {
				t.Error(errDecoder)
				return
			}
			body, err = decoder.DecodeAll(body, nil)
			decoder.Close()
			if err != nil {
				t.Error(err)
				return
			}
		}
		workerCalls.Add(1)
		requests <- exchange{path: r.URL.Path, body: body, headers: r.Header.Clone()}
		if r.Header.Get("Authorization") != "Bearer synthetic-worker-token" {
			t.Error("worker did not use its shared credential")
		}
		if r.Header.Get("Cookie") == "client-cookie" {
			t.Error("caller cookie reached worker upstream")
		}
		w.Header().Set("X-Request-Id", "image-upstream-trace")
		prompt := gjson.GetBytes(body, "prompt").String()
		if prompt == "fail" {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"message":"image quota failure"}}`)
			return
		}
		if prompt == "cancel" {
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		if r.URL.Path == "/v1/responses" {
			if bytes.Contains(body, []byte("fail-before-output")) || bytes.Contains(body, []byte("CPA-owned-retry")) && cpaRetryCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"message":"controlled native pre-output failure","type":"server_error","code":"server_error"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if bytes.Contains(body, []byte("live-tool-stream")) {
				liveStream.serve(r.Context(), func(event []byte) error {
					_, errWrite := fmt.Fprintf(w, "data: %s\n\n", event)
					w.(http.Flusher).Flush()
					return errWrite
				})
				return
			}
			if bytes.Contains(body, []byte("cancel-response")) {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancel\"}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				cancelled <- struct{}{}
				return
			}
			events, _ := responseEvents(body)
			for _, event := range events {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			if bytes.Contains(body, []byte("fail-after-output-open-error")) {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_partial\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"error-before-upstream-EOF\"}}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"real-worker-image","generation_id":"gen_native"}]}`)
	}))
	t.Cleanup(upstream.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	settings := config.CodexRuntimeConfig{Enabled: true, URL: fmt.Sprintf("ws://127.0.0.1:%d/cpa/v1/ws", port)}
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
	cfg.DisableImageGeneration = config.DisableImageGenerationPassthrough
	cfg.RequestLog = true
	auth := runtimeTestAuth("image-account.json")
	runtimeImageAuth(t, cfg, auth)
	auth.Attributes["base_url"] = upstream.URL + "/v1"
	metadata, _ := codexshared.Read(filepath.Join(cfg.AuthDir, auth.ID))
	metadata["id_token"] = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fake-user","email":"fixture@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"image-account","chatgpt_user_id":"fake-user","chatgpt_plan_type":"pro"}}`)) + ".signature"
	metadata["account_id"] = "image-account"
	metadata["refresh_token"] = "unused-synthetic-refresh"
	metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := codexshared.Write(filepath.Join(cfg.AuthDir, auth.ID), metadata); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	accountHome := filepath.Join(state, auth.ID)
	if err := os.MkdirAll(accountHome, 0700); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("model = \"gpt-image-1.5\"\nmodel_provider = \"image_mock\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nchatgpt_base_url = %q\n[model_providers.image_mock]\nname = \"OpenAI\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = true\nrequest_max_retries = 3\nstream_max_retries = 3\n", upstream.URL, upstream.URL+"/v1")
	toml += fmt.Sprintf("supports_websockets = %t\n", nativeTransport != "http")
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), []byte(toml), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "worker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + state, "RUST_LOG=error", "CODEX_CPA_AUTH_DIR=" + cfg.AuthDir, "CODEX_CPA_PORT=" + strconv.Itoa(port), "CODEX_APP_SERVER_MANAGED_CONFIG_PATH=" + filepath.Join(home, "managed.toml"), "CODEX_APP_SERVER_LOGIN_ISSUER=" + upstream.URL, "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + upstream.URL + "/oauth/token"}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("worker diagnostics: %s", data)
		}
	})
	startup := time.NewTimer(45 * time.Second)
	defer startup.Stop()
	ticks := time.NewTicker(20 * time.Millisecond)
	defer ticks.Stop()
	for ready := false; !ready; {
		select {
		case err := <-done:
			t.Fatalf("master exited: %v", err)
		case <-startup.C:
			t.Fatal("master did not become ready")
		case <-ticks.C:
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				_ = conn.Close()
				ready = true
			}
		}
	}
	readExchange := func(t *testing.T) exchange {
		t.Helper()
		select {
		case request := <-requests:
			return request
		case <-time.After(15 * time.Second):
			t.Fatal("native worker did not issue an upstream model request")
			return exchange{}
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
	account := auth.Clone()
	account.Metadata["plan_type"] = "pro"
	account.Metadata["disable_cooling"] = true
	if _, err := manager.Register(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(account.ID, "codex", []*registry.ModelInfo{{ID: "gpt-image-1.5", OwnedBy: "openai"}, {ID: "gpt-6.1-sol", OwnedBy: "openai"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.ID) })
	handler := openaihandlers.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.POST(codexImagesGenerationsPath, handler.ImagesGenerations)
	router.POST(codexImagesEditsPath, handler.ImagesEdits)
	router.POST("/v1/chat/completions", handler.ChatCompletions)
	responsesHandler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router.POST("/v1/responses", responsesHandler.Responses)
	if strings.HasPrefix(nativeTransport, "handshake-") {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("handshake_error_reaches_client/stream=%t", stream), func(t *testing.T) {
				before := websocketAttempts.Load()
				testNativeRuntimeHTTPFailure(t, router, "fail-before-output", stream, "controlled native handshake failure")
				if websocketAttempts.Load() != before+1 || workerCalls.Load() != 0 || cpaCalls.Load() != 0 {
					t.Fatalf("handshake retried or fell back: handshakes=%d worker HTTP=%d CPA=%d", websocketAttempts.Load()-before, workerCalls.Load(), cpaCalls.Load())
				}
			})
		}
		return
	}
	for _, marker := range []string{"fail-before-output", "fail-after-output"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("error_reaches_HTTP_client/%s/stream=%t", marker, stream), func(t *testing.T) {
				before := workerCalls.Load()
				testNativeRuntimeHTTPFailure(t, router, marker, stream, "")
				if workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
					t.Fatalf("native failure was retried: worker=%d CPA=%d", workerCalls.Load()-before, cpaCalls.Load())
				}
				readExchange(t)
			})
		}
	}
	if nativeTransport == "http" {
		t.Run("SSE_error_reaches_client_before_upstream_EOF", func(t *testing.T) {
			before := workerCalls.Load()
			testNativeRuntimeHTTPFailure(t, router, "fail-after-output-open-error", true, "error-before-upstream-EOF")
			if workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
				t.Fatalf("SSE error caused a worker retry: worker=%d CPA=%d", workerCalls.Load()-before, cpaCalls.Load())
			}
			readExchange(t)
		})
	}
	t.Run("CPA_retries_same_worker_with_same_request_ID", func(t *testing.T) {
		manager.SetRetryConfig(1, 0, 0)
		defer manager.SetRetryConfig(0, 0, 0)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			router.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), "same-real-worker-retry")))
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		transport := &http.Transport{}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","stream":true,"input":[{"role":"user","content":"CPA-owned-retry"}]}`))
		req.Header.Set("Content-Type", "application/json")
		before := workerCalls.Load()
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		output, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || !bytes.Contains(output, []byte("response.completed")) || bytes.Contains(output, []byte("controlled native pre-output failure")) {
			t.Fatalf("CPA retry did not deliver the new native result: status=%d body=%s err=%v", response.StatusCode, output, err)
		}
		if workerCalls.Load()-before != 2 || cpaRetryCalls.Load() != 2 || cpaCalls.Load() != 0 {
			t.Fatalf("unexpected retry ownership: worker=%d retry requests=%d CPA=%d", workerCalls.Load()-before, cpaRetryCalls.Load(), cpaCalls.Load())
		}
		readExchange(t)
		readExchange(t)
	})
	t.Run("live_tool_stream_reaches_HTTP_client_before_native_completion", func(t *testing.T) {
		before := workerCalls.Load()
		testNativeRuntimeLiveHTTP(t, router, liveStream, home)
		if workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
			t.Fatalf("unexpected model egress: worker=%d CPA=%d", workerCalls.Load()-before, cpaCalls.Load())
		}
		received := readExchange(t)
		if received.websocket != (nativeTransport == "websocket") {
			t.Fatalf("unexpected native transport: websocket=%t", received.websocket)
		}
	})
	t.Run("HTTP_client_cancels_during_native_generation", func(t *testing.T) {
		before := workerCalls.Load()
		testNativeRuntimeHTTPCancellation(t, router, cancelled)
		if workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
			t.Fatalf("cancellation replayed model work: worker=%d CPA=%d", workerCalls.Load()-before, cpaCalls.Load())
		}
		readExchange(t)
	})
	if nativeTransport == "websocket" {
		t.Run("prefixed_turn_state_survives_live_delivery", func(t *testing.T) {
			beforeCalls, beforeConnections := workerCalls.Load(), websocketAttempts.Load()
			executor := NewCodexAutoExecutor(cfg)
			payload := []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"routing-ticket-probe"}],"client_metadata":{"session_id":"same-source-session","thread_id":"same-source-thread","turn_id":"same-source-turn"}}`)
			for index := range 2 {
				ctx, ginContext := runtimeLoggingContext()
				_, err := executor.Execute(ctx, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: payload}, runtimeTestOptions())
				if err != nil {
					t.Fatal(err)
				}
				received := readExchange(t)
				ticket := gjson.GetBytes(received.body, "client_metadata.x-codex-turn-state").String()
				fields, ok := logging.CodexTurnStateLogFieldsForContext(ginContext)
				if !ok || len(ticket) != index*780 || fields.RequestTurnStateLen != index*780 || fields.ResponseTurnStateLen != 780 || index == 1 && ticket != strings.Repeat("T", 780) {
					t.Fatalf("ticket or correlated log lost: actual=%d log=%+v", len(ticket), fields)
				}
				t.Logf("successful request %d: actual outbound ticket=%d bytes; log=%d/%d", index+1, len(ticket), fields.RequestTurnStateLen, fields.ResponseTurnStateLen)
			}
			if cpaCalls.Load() != 0 || workerCalls.Load()-beforeCalls != 2 || websocketAttempts.Load()-beforeConnections != 1 {
				t.Fatalf("ticket exchange ownership/reuse changed: CPA=%d worker=%d WS=%d", cpaCalls.Load(), workerCalls.Load()-beforeCalls, websocketAttempts.Load()-beforeConnections)
			}
		})
	}
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			for _, form := range []bool{false, true} {
				if form && path != codexImagesEditsPath {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/multipart=%t", path, stream, form), func(t *testing.T) {
					models := []config.PayloadModelRule{{Name: "gpt-image-1.5", Protocol: "openai"}}
					cfg.Payload = config.PayloadConfig{
						Override: []config.PayloadRule{{Models: models, Params: map[string]any{"prompt": "configured-native-prompt", "model": "gpt-image-2.5"}}},
						Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"quality"}}},
					}
					defer func() { cfg.Payload = config.PayloadConfig{} }()
					payload, contentType := nativeRuntimeImagePayload(t, path, stream, form)
					before := workerCalls.Load()
					req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
					req.Header.Set("Content-Type", contentType)
					req.Header.Set("Cookie", "client-cookie")
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), "real-worker-image") || workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
						t.Fatalf("status=%d CPA=%d worker=%d body=%s", rec.Code, cpaCalls.Load(), workerCalls.Load()-before, rec.Body.String())
					}
					received := readExchange(t)
					if received.path != path || gjson.GetBytes(received.body, "prompt").String() != "configured-native-prompt" || gjson.GetBytes(received.body, "model").String() != "gpt-image-2.5" || gjson.GetBytes(received.body, "quality").Exists() {
						t.Fatalf("native image DTO lost supported business input: path=%s body=%s", received.path, received.body)
					}
					if gjson.GetBytes(received.body, "stream").Exists() {
						t.Fatal("downstream streaming mode changed the native Images DTO")
					}
					if path == codexImagesEditsPath && (gjson.GetBytes(received.body, "images.#").Int() != 1 || !strings.HasSuffix(gjson.GetBytes(received.body, "images.0.image_url").String(), "AQID")) {
						t.Fatal("native image edit lost its input image", string(received.body))
					}
					if stream && !strings.Contains(rec.Body.String(), "completed") {
						t.Fatal("native final image was not adapted to the client event stream")
					}
					t.Logf("CPA model requests=0; native worker requests=1; operation=%s", received.path)
				})
			}
		}
	}
	t.Run("unsupported_image_inputs_are_explicit", func(t *testing.T) {
		for _, field := range []string{"mask", "partial_images", "output_format", "extension"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t", field, stream), func(t *testing.T) {
					payload, _ := nativeRuntimeImagePayload(t, codexImagesEditsPath, stream, false)
					var value any = "unsupported"
					if field == "mask" {
						value = map[string]string{"image_url": "data:image/png;base64,BAUG"}
					} else if field == "partial_images" {
						value = 1
					} else if field == "output_format" {
						value = "jpeg"
					}
					payload, _ = sjson.SetBytes(payload, field, value)
					before := workerCalls.Load()
					_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, codexOpenAIImageTestOptions(codexImagesEditsPath, stream), stream)
					if err == nil || !strings.Contains(err.Error(), field) || workerCalls.Load() != before || cpaCalls.Load() != 0 {
						t.Fatalf("unsupported input was dropped or sent: field=%s worker=%d CPA=%d error=%v", field, workerCalls.Load()-before, cpaCalls.Load(), err)
					}
				})
			}
		}
	})
	for _, mode := range []string{"summary", "native_lite", "compaction_trigger", "hosted_image", "tools", "tool_results"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses/%s/stream=%t", mode, stream), func(t *testing.T) {
				payload := []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"summarize this conversation"}]}`)
				switch mode {
				case "native_lite":
					payload = []byte(`{"model":"gpt-6.1-sol","reasoning":{"context":"all_turns"},"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]},{"role":"user","content":"native lite input"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
				case "compaction_trigger":
					payload = []byte(`{"model":"gpt-6.1-sol","reasoning":{"context":"all_turns"},"input":[{"role":"user","content":"history"},{"type":"compaction_trigger"}]}`)
				case "hosted_image":
					payload = []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"draw-hosted"}],"tools":[{"type":"image_generation","quality":"high"}]}`)
				case "tools":
					payload = []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"tool-fixture"}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}},{"type":"custom","name":"exec","format":{"type":"text"}},{"type":"custom","name":"apply_patch","format":{"type":"text"}}]}`)
				case "tool_results":
					payload = []byte(`{"model":"gpt-6.1-sol","input":[{"type":"function_call","call_id":"call_shell","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"call_shell","output":"client shell result"},{"type":"custom_tool_call","call_id":"call_exec","name":"exec","input":"hello"},{"type":"custom_tool_call_output","call_id":"call_exec","output":"client exec result"}]}`)
				}
				for key, value := range map[string]string{"session_id": "client-session", "thread_id": "client-thread", "turn_id": mode + "-turn"} {
					payload, _ = sjson.SetBytes(payload, "client_metadata."+key, value)
				}
				if mode == "summary" {
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "gpt-6.1-sol", Protocol: "codex"}},
						Params: map[string]any{"instructions": "native-supported-instructions", "service_tier": "priority", "reasoning.effort": "low", "text.verbosity": "low"},
					}}}
					defer func() { cfg.Payload = config.PayloadConfig{} }()
				}
				ctx, ginContext := runtimeLoggingContext()
				trace := fmt.Sprintf("native-%s-%t", mode, stream)
				ctx = logging.WithRequestID(ctx, trace)
				before := workerCalls.Load()
				output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: payload}, runtimeTestOptions(), stream)
				attempts := workerCalls.Load() - before
				if err != nil || attempts != 1 || cpaCalls.Load() != 0 {
					t.Fatalf("native Responses route failed: err=%v worker=%d CPA=%d", err, attempts, cpaCalls.Load())
				}
				received := readExchange(t)
				if received.path != "/v1/responses" || received.websocket != (nativeTransport == "websocket") || received.websocket && gjson.GetBytes(received.body, "type").String() != "response.create" || !received.websocket && !gjson.GetBytes(received.body, "stream").Bool() {
					t.Fatalf("native Responses request was not built: %s %s", received.path, received.body)
				}
				if mode == "summary" {
					if !bytes.Contains(received.body, []byte("native-supported-instructions")) || gjson.GetBytes(received.body, "service_tier").String() != "priority" || gjson.GetBytes(received.body, "reasoning.effort").String() != "low" || gjson.GetBytes(received.body, "text.verbosity").String() != "low" {
						t.Fatalf("supported CPA payload rules were lost at the native entry: %s", received.body)
					}
					for _, item := range gjson.GetBytes(received.body, "input").Array() {
						if item.Get("role").String() == "user" && (item.Get("type").String() != "message" || item.Get("content.0.type").String() != "input_text") {
							t.Fatalf("worker did not normalize native message input: %s", item.Raw)
						}
					}
				}
				if mode == "native_lite" && (gjson.GetBytes(received.body, "reasoning.context").String() != "all_turns" || !bytes.Contains(received.body, []byte("additional_tools"))) {
					t.Fatalf("native Lite context or tool history was lost: %s", received.body)
				}
				if mode == "compaction_trigger" && (!bytes.Contains(received.body, []byte("compaction_trigger")) || !bytes.Contains(output, []byte("compact-result"))) {
					t.Fatal("ordinary Responses compaction was lost")
				}
				if stream && !bytes.Contains(output, []byte("response.future_extension")) {
					t.Fatal("nonterminal extension event was lost by response adaptation")
				}
				if mode == "hosted_image" {
					if !bytes.Contains(received.body, []byte("additional_tools")) || !bytes.Contains(received.body, []byte("image_generation")) || !bytes.Contains(received.body, []byte(`"quality":"high"`)) || !bytes.Contains(output, []byte("native-hosted-image")) {
						t.Fatalf("native hosted tool mapping lost its input or output: request=%s result=%s", received.body, output)
					}
					if stream && !bytes.Contains(output, []byte("hosted-partial")) {
						t.Fatal("hosted partial image event was lost")
					}
				}
				if mode == "tools" {
					for _, call := range []string{"call_shell", "call_exec", "call_patch"} {
						if !bytes.Contains(output, []byte(call)) {
							t.Fatalf("client tool call missing: %s, body=%s", call, output)
						}
					}
					if _, errStat := os.Stat(filepath.Join(home, "must-not-be-created")); !os.IsNotExist(errStat) {
						t.Fatalf("server executed a client tool: %v", errStat)
					}
				}
				if mode == "tool_results" && (!bytes.Contains(received.body, []byte("client shell result")) || !bytes.Contains(received.body, []byte("client exec result"))) {
					t.Fatal("caller-owned tool results were lost")
				}
				requestLog := runtimeLogText(t, ginContext, "API_REQUEST")
				target := upstream.URL + received.path
				if received.websocket {
					target = "ws" + strings.TrimPrefix(upstream.URL, "http") + received.path
				}
				if !strings.Contains(requestLog, target) {
					t.Fatal("actual native request missing from CPA logs")
				}
				workerLog, _ := os.ReadFile(logPath)
				if !bytes.Contains(workerLog, []byte(`"rpcRequestId":"`+trace+`"`)) {
					t.Fatal("CPA/master/worker correlation missing")
				}
			})
		}
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("public_chat/stream=%t", stream), func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"chat fixture"}],"stream":%t}`, stream))
			before := workerCalls.Load()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 200 || workerCalls.Load() != before+1 || cpaCalls.Load() != 0 || !strings.Contains(rec.Body.String(), "native result") {
				t.Fatalf("Chat did not execute through native runtime: status=%d native=%d CPA=%d body=%s", rec.Code, workerCalls.Load()-before, cpaCalls.Load(), rec.Body.String())
			}
			if !stream && gjson.Get(rec.Body.String(), "choices.0.message.content").String() != "native result" {
				t.Fatal("native output was not translated to Chat completions", rec.Body.String())
			}
			if stream && !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
				t.Fatal("native output was not translated to Chat SSE", rec.Body.String())
			}
			received := readExchange(t)
			if received.path != "/v1/responses" || received.websocket != (nativeTransport == "websocket") || !bytes.Contains(received.body, []byte("chat fixture")) {
				t.Fatal("Chat input did not reach the native model request", string(received.body))
			}
		})
	}
	t.Run("standalone_compact_rejected", func(t *testing.T) {
		before := workerCalls.Load()
		opts := runtimeTestOptions()
		opts.Alt = "responses/compact"
		_, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":[]}`)}, opts)
		if err == nil || workerCalls.Load() != before || cpaCalls.Load() != 0 {
			t.Fatalf("standalone compact sent an upstream request: %v", err)
		}
	})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_failure/stream=%t", stream), func(t *testing.T) {
			before := workerCalls.Load()
			_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"fail"}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
			if err == nil || workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
				t.Fatalf("native failure replayed by CPA: worker=%d CPA=%d err=%v", workerCalls.Load()-before, cpaCalls.Load(), err)
			}
			readExchange(t)
		})
		t.Run(fmt.Sprintf("cancel/stream=%t", stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"cancel"}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
				result <- err
			}()
			readExchange(t)
			cancel()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("cancellation reported success")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("CPA did not propagate cancellation")
			}
			select {
			case <-cancelled:
			case <-time.After(15 * time.Second):
				t.Fatal("native upstream call survived cancellation")
			}
		})
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("responses_cancel/stream=%t", stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"cancel-response"}]}`)}, runtimeTestOptions(), stream)
				result <- err
			}()
			readExchange(t)
			cancel()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("cancelled Responses request reported success")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("CPA did not cancel the native Responses call")
			}
			select {
			case <-cancelled:
			case <-time.After(15 * time.Second):
				t.Fatal("native Responses transport survived cancellation")
			}
		})
	}
	t.Run("mixed_account_pool_does_not_retry_locally", func(t *testing.T) {
		mixed := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		mixed.SetConfig(cfg)
		mixed.SetRetryConfig(2, 0, 0)
		mixed.RegisterExecutor(NewCodexAutoExecutor(cfg))
		account := auth.Clone()
		account.Metadata["disable_cooling"] = true
		account.Attributes["priority"] = "10"
		local := &coreauth.Auth{ID: "local-fallback.json", Provider: "codex", Metadata: map[string]any{"access_token": "local-token", "disable_cooling": true}, Attributes: map[string]string{"base_url": upstream.URL + "/v1", "priority": "0"}}
		for _, entry := range []*coreauth.Auth{account, local} {
			if _, err := mixed.Register(context.Background(), entry); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(entry.ID, "codex", []*registry.ModelInfo{{ID: "gpt-image-1.5", OwnedBy: "openai"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(entry.ID) })
		}
		for _, stream := range []bool{false, true} {
			before := workerCalls.Load()
			req := coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"fail"}`)}
			opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
			var err error
			if stream {
				var result *coreexecutor.StreamResult
				result, err = mixed.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = mixed.Execute(context.Background(), []string{"codex"}, req, opts)
			}
			if err == nil || cpaCalls.Load() != 0 || workerCalls.Load() != before+3 {
				t.Fatalf("mixed pool ignored CPA retry budget or used direct egress: worker=%d CPA=%d err=%v", workerCalls.Load()-before, cpaCalls.Load(), err)
			}
			for range 3 {
				readExchange(t)
			}
		}
	})
	client, err := bridge.Dial(context.Background(), settings.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call("cpa/credential/reload", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if cpaCalls.Load() != 0 {
		t.Fatal("CPA issued a model upstream request")
	}
	if nativeTransport != "http" && websocketAttempts.Load() == 0 {
		t.Fatal("native WebSocket configuration was ignored")
	}

}

func nativeRuntimeImagePayload(t *testing.T, path string, stream, form bool) ([]byte, string) {
	t.Helper()
	if !form {
		payload := map[string]any{"model": "gpt-image-1.5", "prompt": "draw", "quality": "low", "stream": stream}
		if path == codexImagesEditsPath {
			payload["images"] = []map[string]string{{"image_url": "data:image/png;base64,AQID"}}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return body, "application/json"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"model": "gpt-image-1.5", "prompt": "draw", "quality": "low", "stream": strconv.FormatBool(stream)} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image[]", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func nativeRuntimeResponseEvents(t *testing.T, home string, body []byte) [][]byte {
	t.Helper()
	output := `[{"type":"message","id":"msg_native","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native result"}]}]`
	if bytes.Contains(body, []byte("compaction_trigger")) {
		output = `[{"type":"compaction","encrypted_content":"compact-result"}]`
	}
	if bytes.Contains(body, []byte("draw-hosted")) {
		output = `[{"type":"image_generation_call","status":"completed","result":"native-hosted-image"}]`
	}
	if bytes.Contains(body, []byte("tool-fixture")) {
		marker := filepath.Join(home, "must-not-be-created")
		calls, errMarshal := json.Marshal([]map[string]any{
			{"type": "function_call", "id": "fc_shell", "call_id": "call_shell", "name": "shell", "arguments": fmt.Sprintf(`{"command":"touch %s"}`, marker), "status": "completed"},
			{"type": "custom_tool_call", "id": "ctc_exec", "call_id": "call_exec", "name": "exec", "input": "touch " + marker, "status": "completed"},
			{"type": "custom_tool_call", "id": "ctc_patch", "call_id": "call_patch", "name": "apply_patch", "input": "*** Begin Patch\n*** Add File: " + marker + "\n+forbidden\n*** End Patch", "status": "completed"},
		})
		if errMarshal != nil {
			t.Error(errMarshal)
			return nil
		}
		output = string(calls)
	}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_native"}},
		{"type": "response.future_extension", "future": map[string]any{"kept": true}},
	}
	if bytes.Contains(body, []byte("draw-hosted")) {
		events = append(events, map[string]any{"type": "response.image_generation_call.partial_image", "partial_image_b64": "hosted-partial", "partial_image_index": 0})
	}
	for index, item := range gjson.Parse(output).Array() {
		added := []byte(item.Raw)
		if item.Get("type").String() == "message" {
			added, _ = sjson.SetBytes(added, "content", []any{})
		}
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": index, "item": json.RawMessage(added)})
		if item.Get("type").String() == "message" {
			events = append(events, map[string]any{"type": "response.output_text.delta", "output_index": index, "content_index": 0, "item_id": item.Get("id").String(), "delta": item.Get("content.0.text").String()})
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": json.RawMessage(item.Raw)})
	}
	events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_native", "status": "completed", "output": json.RawMessage(output),
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
	}})
	var result [][]byte
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Error(err)
			return nil
		}
		result = append(result, encoded)
	}
	return result
}

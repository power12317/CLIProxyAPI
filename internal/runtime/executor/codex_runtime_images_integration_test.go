package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
	binary := os.Getenv("CODEX_RUNTIME_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_TEST_BINARY to the image-capable codex-app-server")
	}
	home := t.TempDir()
	type exchange struct {
		path    string
		body    []byte
		headers http.Header
	}
	requests := make(chan exchange, 32)
	cancelled := make(chan struct{}, 4)
	var workerCalls, cpaCalls atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
		cpaCalls.Add(1)
		return nil, fmt.Errorf("CPA direct upstream request forbidden")
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/v1/images/") && r.URL.Path != "/v1/responses" {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		workerCalls.Add(1)
		requests <- exchange{r.URL.Path, body, r.Header.Clone()}
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
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"part\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			output := `[{"type":"image_generation_call","result":"real-worker-image","output_format":"png"}]`
			if bytes.Contains(body, []byte("compaction_trigger")) {
				output = `[{"type":"compaction","encrypted_content":"compact-result"}]`
			}
			if value := gjson.GetBytes(body, "stream"); value.Exists() && !value.Bool() {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"response-image","status":"completed","output":%s}`, output)
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-image\",\"status\":\"completed\",\"output\":%s}}\n\n", output)
			return
		}
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			kind := "image_generation"
			if r.URL.Path == "/v1/images/edits" {
				kind = "image_edit"
			}
			_, _ = fmt.Fprintf(w, "event: %s.partial_image\ndata: {\"type\":\"%s.partial_image\",\"b64_json\":\"part\"}\n\nevent: %s.completed\ndata: {\"type\":\"%s.completed\",\"b64_json\":\"real-worker-image\"}\n\n", kind, kind, kind, kind)
		} else {
			_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"real-worker-image"}]}`)
		}
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
	toml := fmt.Sprintf("model = \"gpt-image-1.5\"\nmodel_provider = \"image_mock\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nchatgpt_base_url = %q\n[model_providers.image_mock]\nname = \"OpenAI\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", upstream.URL, upstream.URL+"/v1")
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
	requestIndex := 0
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			for _, form := range []bool{false, true} {
				if form && path != codexImagesEditsPath {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/multipart=%t", path, stream, form), func(t *testing.T) {
					requestIndex++
					trace := fmt.Sprintf("image-real-%d", requestIndex)
					ctx, ginContext := runtimeLoggingContext()
					ctx = logging.WithRequestID(ctx, trace)
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
						cpaCalls.Add(1)
						return nil, fmt.Errorf("CPA direct call forbidden")
					})))
					payload, contentType := runtimeImagePayload(t, form)
					opts := codexOpenAIImageTestOptions(path, stream)
					opts.Headers = http.Header{"Content-Type": {contentType}, "Cookie": {"client-cookie"}}
					models := []config.PayloadModelRule{{Name: "gpt-image-1.5", Protocol: "openai"}}
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"prompt": "final-prompt", "model": "gpt-image-2.5", "extension.number": 7}}}, Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"quality", "prompt_cache_key"}}}}
					before := workerCalls.Load()
					output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, opts, stream)
					if err != nil {
						t.Fatal(err)
					}
					received := <-requests
					if received.path != strings.TrimPrefix(path, "/v1") && received.path != "/v1"+strings.TrimPrefix(path, "/v1") {
						t.Fatal(received.path)
					}
					if gjson.GetBytes(received.body, "prompt").String() != "final-prompt" || gjson.GetBytes(received.body, "model").String() != "gpt-image-2.5" || gjson.GetBytes(received.body, "quality").Exists() || gjson.GetBytes(received.body, "prompt_cache_key").Exists() || gjson.GetBytes(received.body, "extension.number").Int() != 7 {
						t.Fatalf("worker lost image request values: %s", received.body)
					}
					if form && (gjson.GetBytes(received.body, "images.#").Int() != 2 || !strings.HasSuffix(gjson.GetBytes(received.body, "mask.image_url").String(), "BAUG") || gjson.GetBytes(received.body, "extra.#").Int() != 2) {
						t.Fatal("worker lost images/mask/duplicate fields")
					}
					if cpaCalls.Load() != 0 || workerCalls.Load() != before+1 || !bytes.Contains(output, []byte("real-worker-image")) {
						t.Fatalf("CPA=%d worker=%d result=%s", cpaCalls.Load(), workerCalls.Load()-before, output)
					}
					requestLog := runtimeLogText(t, ginContext, "API_REQUEST")
					if !strings.Contains(requestLog, upstream.URL+received.path) || !strings.Contains(requestLog, "final-prompt") {
						t.Fatal("CPA log did not record actual worker request")
					}
					workerLog, _ := os.ReadFile(logPath)
					if !bytes.Contains(workerLog, []byte(`"rpcRequestId":"`+trace+`"`)) {
						t.Fatal("master/worker request correlation missing")
					}
					t.Logf("CPA model HTTP=%d worker model HTTP=%d trace=%s", cpaCalls.Load(), workerCalls.Load()-before, trace)
				})
			}
		}
	}
	cfg.Payload = config.PayloadConfig{}
	t.Run("payload_stream_override_for_nonstream_client", func(t *testing.T) {
		models := []config.PayloadModelRule{{Name: "gpt-image-1.5", Protocol: "openai"}}
		cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"stream": true}}}}
		defer func() { cfg.Payload = config.PayloadConfig{} }()
		payload, _ := runtimeImagePayload(t, false)
		output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, false), false)
		if err != nil {
			t.Fatal(err)
		}
		received := <-requests
		if !gjson.GetBytes(received.body, "stream").Bool() || gjson.GetBytes(output, "data.0.b64_json").String() != "real-worker-image" || !gjson.GetBytes(output, "created").Exists() {
			t.Fatal("stream override or image JSON conversion lost")
		}
	})
	t.Run("public_HTTP_endpoints", func(t *testing.T) {
		manager := coreauth.NewManager(nil, nil, nil)
		manager.SetConfig(cfg)
		manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
		account := auth.Clone()
		account.Metadata["plan_type"] = "pro"
		if _, err := manager.Register(context.Background(), account); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(account.ID, "codex", []*registry.ModelInfo{{ID: "gpt-image-1.5", OwnedBy: "openai"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.ID) })
		handler := openaihandlers.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
		router := gin.New()
		router.POST(codexImagesGenerationsPath, handler.ImagesGenerations)
		router.POST(codexImagesEditsPath, handler.ImagesEdits)
		for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
			for _, stream := range []bool{false, true} {
				for _, form := range []bool{false, true} {
					if form && path != codexImagesEditsPath {
						continue
					}
					t.Run(fmt.Sprintf("%s/stream=%t/multipart=%t", path, stream, form), func(t *testing.T) {
						payload, contentType := runtimeImagePayload(t, form)
						if stream {
							if form {
								var err error
								payload, contentType, err = prepareOpenAICompatImagesPayload(payload, "gpt-image-1.5", contentType, true)
								if err != nil {
									t.Fatal(err)
								}
							} else {
								payload, _ = sjson.SetBytes(payload, "stream", true)
							}
						}
						before := workerCalls.Load()
						req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
						req.Header.Set("Content-Type", contentType)
						rec := httptest.NewRecorder()
						router.ServeHTTP(rec, req)
						if rec.Code != 200 || !strings.Contains(rec.Body.String(), "real-worker-image") || workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
							t.Fatalf("status=%d CPA=%d worker=%d body=%s", rec.Code, cpaCalls.Load(), workerCalls.Load()-before, rec.Body.String())
						}
						<-requests
					})
				}
			}
		}
	})
	t.Run("payload_filters_edit_content", func(t *testing.T) {
		models := []config.PayloadModelRule{{Name: "gpt-image-1.5", Protocol: "openai"}}
		cfg.Payload = config.PayloadConfig{
			Override: []config.PayloadRule{{Models: models, Params: map[string]any{"stream": false}}},
			Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"mask", "images.1", "quality"}}},
		}
		defer func() { cfg.Payload = config.PayloadConfig{} }()
		payload, contentType := runtimeImagePayload(t, true)
		opts := codexOpenAIImageTestOptions(codexImagesEditsPath, true)
		opts.Headers = http.Header{"Content-Type": {contentType}}
		output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		received := <-requests
		for _, field := range []string{"mask", "quality"} {
			if gjson.GetBytes(received.body, field).Exists() {
				t.Fatalf("image content filter lost: %s", field)
			}
		}
		if gjson.GetBytes(received.body, "images.#").Int() != 1 || !bytes.Contains(output, []byte("image_edit.completed")) {
			t.Fatal("filtered files or JSON-to-stream response was not preserved")
		}
	})
	for _, mode := range []string{"image_responses", "image_responses_native_stream", "image_responses_native_normalization", "builtin_image_tool", "compaction_trigger", "summary"} {
		t.Run(mode, func(t *testing.T) {
			before := workerCalls.Load()
			req := coreexecutor.Request{Model: "gpt-5.4-mini", Payload: []byte(`{"model":"gpt-5.4-mini","input":[{"role":"user","content":"summarize this conversation"}]}`)}
			opts := runtimeTestOptions()
			if strings.HasPrefix(mode, "image_responses") {
				req = coreexecutor.Request{Model: "image-alias", Payload: []byte(`{"model":"image-alias","prompt":"draw"}`)}
				opts = codexOpenAIImageTestOptions(codexImagesGenerationsPath, false)
				if mode == "image_responses_native_normalization" {
					models := []config.PayloadModelRule{{Name: "gpt-5.4-mini", Protocol: "codex"}}
					cfg.Payload = config.PayloadConfig{
						Override: []config.PayloadRule{{Models: models, Params: map[string]any{"input.0.content": "draw"}}},
						Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"input.0.type", "instructions", "store", "parallel_tool_calls"}}},
					}
					defer func() { cfg.Payload = config.PayloadConfig{} }()
				}
				if mode == "image_responses_native_stream" {
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4-mini", Protocol: "codex"}}, Params: map[string]any{"stream": false}}}}
					defer func() { cfg.Payload = config.PayloadConfig{} }()
				}
			} else if mode == "builtin_image_tool" {
				req.Payload = []byte(`{"model":"gpt-5.4-mini","input":[{"role":"user","content":"draw"}],"tools":[{"type":"image_generation"}]}`)
			} else if mode == "compaction_trigger" {
				req.Payload = []byte(`{"model":"gpt-5.4-mini","input":[{"role":"user","content":"history"},{"type":"compaction_trigger"}]}`)
			}
			result, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			received := <-requests
			if received.path != "/v1/responses" || cpaCalls.Load() != 0 || workerCalls.Load() != before+1 {
				t.Fatal("Responses regression used wrong transport")
			}
			if mode == "compaction_trigger" && (!bytes.Contains(received.body, []byte("compaction_trigger")) || !bytes.Contains(result.Payload, []byte("compact-result"))) {
				t.Fatal("normal Responses compaction was lost")
			}
			if strings.HasPrefix(mode, "image_responses") && gjson.GetBytes(result.Payload, "data.0.b64_json").String() != "real-worker-image" {
				t.Fatalf("native Responses image result lost: %s", result.Payload)
			}
			if mode == "image_responses_native_stream" && !gjson.GetBytes(received.body, "stream").Bool() {
				t.Fatalf("worker native streaming was overridden by CPA input: %s", received.body)
			}
			if mode == "image_responses_native_normalization" {
				if gjson.GetBytes(received.body, "input.0.type").String() != "message" || gjson.GetBytes(received.body, "input.0.content.0.type").String() != "input_text" || gjson.GetBytes(received.body, "input.0.content.0.text").String() != "draw" {
					t.Fatalf("worker did not normalize the message: %s", received.body)
				}
				if gjson.GetBytes(received.body, "instructions").Exists() || gjson.GetBytes(received.body, "store").Type != gjson.False || gjson.GetBytes(received.body, "parallel_tool_calls").Type != gjson.True {
					t.Fatalf("worker native defaults were suppressed: %s", received.body)
				}
				t.Log("worker normalized message type/content and applied native Responses defaults")
			}
		})
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_failure/stream=%t", stream), func(t *testing.T) {
			before := workerCalls.Load()
			_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"fail"}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
			if err == nil || !strings.Contains(err.Error(), "image quota failure") || workerCalls.Load() != before+1 || cpaCalls.Load() != 0 {
				t.Fatalf("unexpected failure/retry: %v", err)
			}
			<-requests
		})
		t.Run(fmt.Sprintf("cancel/stream=%t", stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"cancel","stream":true}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
				result <- err
			}()
			select {
			case <-requests:
				cancel()
			case <-time.After(15 * time.Second):
				t.Fatal("worker did not send cancellation fixture")
			}
			select {
			case <-result:
			case <-time.After(15 * time.Second):
				t.Fatal("CPA did not stop after cancellation")
			}
			select {
			case <-cancelled:
			case <-time.After(15 * time.Second):
				t.Fatal("worker upstream request survived cancellation")
			}
		})
	}
	t.Run("mixed_account_pool_does_not_retry_locally", func(t *testing.T) {
		manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		manager.SetConfig(cfg)
		manager.SetRetryConfig(2, 0, 0)
		manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
		account := auth.Clone()
		account.Metadata["disable_cooling"] = true
		account.Attributes["priority"] = "10"
		local := &coreauth.Auth{ID: "local-fallback.json", Provider: "codex", Metadata: map[string]any{"access_token": "local-token", "disable_cooling": true}, Attributes: map[string]string{"base_url": upstream.URL + "/v1", "priority": "0"}}
		for _, entry := range []*coreauth.Auth{account, local} {
			if _, err := manager.Register(context.Background(), entry); err != nil {
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
				_, err = manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
			} else {
				_, err = manager.Execute(context.Background(), []string{"codex"}, req, opts)
			}
			if err == nil || cpaCalls.Load() != 0 || workerCalls.Load() != before+1 {
				t.Fatalf("mixed-pool fallback reached CPA: worker=%d CPA=%d err=%v", workerCalls.Load()-before, cpaCalls.Load(), err)
			}
			<-requests
		}
	})
	if cpaCalls.Load() != 0 {
		t.Fatal("CPA issued a model request")
	}
	client, err := bridge.Dial(context.Background(), settings.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call("cpa/credential/reload", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
}

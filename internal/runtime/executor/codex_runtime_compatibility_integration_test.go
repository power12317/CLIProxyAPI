package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The real main-branch master/worker sends to a local upstream. Any model HTTP
// request in CPA fails independently of the response-content assertions.
func TestCodexRuntimeCompatibleRealWorker(t *testing.T) {
	binary := os.Getenv("CODEX_RUNTIME_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_TEST_BINARY to the main-branch codex-app-server")
	}
	home := t.TempDir()
	requests := make(chan []byte, 32)
	phase := make(chan []byte)
	cancelled := make(chan struct{}, 1)
	var cpaCalls, workerCalls atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
		cpaCalls.Add(1)
		return nil, fmt.Errorf("CPA model egress forbidden")
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
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
			defer decoder.Close()
			body, err = decoder.DecodeAll(body, nil)
			if err != nil {
				t.Error(err)
				return
			}
		}
		workerCalls.Add(1)
		requests <- body
		lite := r.Header.Get("X-OpenAI-Internal-Codex-Responses-Lite") == "true"
		if lite && gjson.GetBytes(body, "parallel_tool_calls").Type != gjson.False ||
			bytes.Contains(body, []byte("native-lite-parallel-probe")) && !lite {
			t.Error("worker sent an invalid native Lite/parallel combination")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"native Lite requires parallel_tool_calls=false"}}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-worker-token" {
			t.Error("model request did not use worker credentials")
		}
		if bytes.Contains(body, []byte("main-upstream-failure")) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"main-upstream-quota"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(event []byte) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
		if bytes.Contains(body, []byte("main-live-stream")) {
			for {
				select {
				case event := <-phase:
					write(event)
					if gjson.GetBytes(event, "type").String() == "response.completed" {
						return
					}
				case <-r.Context().Done():
					return
				}
			}
		}
		if bytes.Contains(body, []byte("main-cancel-request")) {
			write([]byte(`{"type":"response.created","response":{"id":"resp_cancel","status":"in_progress"}}`))
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		output := `[{"type":"message","id":"msg_main","role":"assistant","content":[{"type":"output_text","text":"main worker result"}]}]`
		if bytes.Contains(body, []byte("main-client-tool-only")) {
			call, _ := json.Marshal(map[string]any{"type": "custom_tool_call", "id": "ctc_client", "call_id": "call_client", "name": "run_source", "input": "touch " + filepath.Join(home, "must-not-be-created"), "status": "completed"})
			output = "[" + string(call) + "]"
		}
		write([]byte(`{"type":"response.created","response":{"id":"resp_main","status":"in_progress"}}`))
		if !bytes.Contains(body, []byte("main-client-tool-only")) {
			write([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_main","type":"message","role":"assistant","content":[]}}`))
			write([]byte(`{"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg_main","part":{"type":"output_text","text":"","annotations":[]}}`))
			write([]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_main","delta":"main worker result"}`))
			write([]byte(`{"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"msg_main","text":"main worker result"}`))
			write([]byte(`{"type":"response.content_part.done","output_index":0,"content_index":0,"item_id":"msg_main","part":{"type":"output_text","text":"main worker result","annotations":[]}}`))
			write([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_main","type":"message","role":"assistant","content":[{"type":"output_text","text":"main worker result"}]}}`))
		}
		write([]byte(`{"type":"response.completed","response":{"id":"resp_main","model":"gpt-6.1-sol","status":"completed","output":` + output + `,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`))
	}))
	defer upstream.Close()
	cfg, auth := startCompatibleMainWorker(t, binary, home, upstream.URL)
	testContext := context.WithValue(t.Context(), "cliproxy.roundtripper", http.DefaultTransport)
	read := func(t *testing.T) []byte {
		t.Helper()
		select {
		case body := <-requests:
			return body
		case <-time.After(15 * time.Second):
			t.Fatal("worker did not send a model request")
			return nil
		}
	}
	schema := `{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"code":{"type":"string","pattern":"\\p{L}+","default":"UPPER"},"large":{"type":"integer","maximum":9007199254740993}},"additionalProperties":false}`
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("controls/%s/stream=%t", source, stream), func(t *testing.T) {
				tool := []byte(`{"type":"function","name":"query_inventory","parameters":` + schema + `,"strict":false,"output_schema":{"type":"string"}}`)
				body := []byte(`{"input":"main compatible caller","tool_choice":"none","parallel_tool_calls":false,"include":[],"store":false,"background":false,"temperature":0,"top_p":0.7,"max_output_tokens":120,"service_tier":"flex","client_metadata":{"optional_trace":{"caller":"WorkBuddy"},"x-codex-client-timing":{"old":true}},"text":{"format":{"type":"json_schema","name":"inventory_contract","strict":false,"schema":` + schema + `}}}`)
				if source == sdktranslator.FormatOpenAI || source == sdktranslator.FormatClaude {
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"main compatible caller"}]`))
				}
				if source == sdktranslator.FormatOpenAI {
					function, _ := sjson.DeleteBytes(tool, "type")
					tool = []byte(`{"type":"function","function":` + string(function) + `}`)
				}
				if source == sdktranslator.FormatClaude {
					body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"none"}`))
					body, _ = sjson.SetRawBytes(body, "output_config.format", []byte(gjson.GetBytes(body, "text.format").Raw))
					tool, _ = sjson.SetRawBytes(tool, "input_schema", []byte(schema))
					tool, _ = sjson.DeleteBytes(tool, "parameters")
				}
				if source == sdktranslator.FormatGemini {
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "contents", []byte(`[{"role":"user","parts":[{"text":"main compatible caller"}]}]`))
					tool = []byte(`{"functionDeclarations":[` + string(tool) + `]}`)
				}
				body, _ = sjson.SetRawBytes(body, "tools", []byte("["+string(tool)+"]"))
				opts := runtimeTestOptions()
				opts.SourceFormat = source
				before := workerCalls.Load()
				output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), testContext, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, opts, stream)
				if err != nil || !bytes.Contains(output, []byte("main worker result")) {
					t.Fatalf("main compatible input failed: %v %s", err, output)
				}
				actual := read(t)
				for key, expected := range map[string]string{"tool_choice": `"none"`, "parallel_tool_calls": "false", "include": "[]", "store": "false", "temperature": "0", "top_p": "0.7", "max_output_tokens": "120", "service_tier": `"flex"`} {
					equalRuntimeJSON(t, []byte(expected), []byte(gjson.GetBytes(actual, key).Raw))
				}
				declaration := mainRuntimeToolDeclarations(actual)["query_inventory"]
				if declaration.Get("strict").Type != gjson.False || declaration.Get("output_schema.type").String() != "string" || declaration.Get("parameters.properties.large.maximum").Raw != "9007199254740993" {
					t.Fatalf("tool declaration lost: %s", declaration.Raw)
				}
				equalRuntimeJSON(t, []byte(schema), []byte(declaration.Get("parameters").Raw))
				if gjson.GetBytes(actual, "text.format.name").String() != "inventory_contract" || gjson.GetBytes(actual, "text.format.strict").Type != gjson.False {
					t.Fatalf("structured output contract lost: %s", actual)
				}
				if gjson.GetBytes(actual, "client_metadata.x-codex-client-timing.old").Bool() {
					t.Fatal("stale transport timing was forwarded")
				}
				if cpaCalls.Load() != 0 || workerCalls.Load() != before+1 {
					t.Fatalf("sender ownership changed: CPA=%d worker=%d", cpaCalls.Load(), workerCalls.Load()-before)
				}
			})
		}
	}
	for _, test := range []struct {
		name, body string
		check      func(*testing.T, []byte)
	}{
		{
			name: "namespace_and_inferred_tool_result",
			body: `{"input":[{"role":"assistant","content":"display history","status":"completed","metadata":{"ui":true}},{"type":"function_call","name":"lookup","namespace":"warehouse","call_id":"prior_call","arguments":"{}"},{"type":"function_call_output","name":"lookup","namespace":"warehouse","output":"found"},{"role":"user","content":"continue"}],"tools":[{"type":"namespace","name":"warehouse","description":"client tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":false}]}],"tool_choice":{"type":"function","namespace":"warehouse","name":"lookup"}}`,
			check: func(t *testing.T, actual []byte) {
				declaration := mainRuntimeToolDeclarations(actual)["warehouse"]
				if declaration.Get("tools.0.name").String() != "lookup" || gjson.GetBytes(actual, "tool_choice.namespace").String() != "warehouse" {
					t.Fatalf("namespace declaration or selection was lost: %s", actual)
				}
				for _, item := range gjson.GetBytes(actual, "input").Array() {
					if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "prior_call" && item.Get("output").String() == "found" {
						return
					}
				}
				t.Fatalf("unambiguous result association was not inferred: %s", actual)
			},
		},
		{
			name: "function_names_are_client_declarations",
			body: `{"input":"declared functions","tools":[{"type":"function","name":"WebSearch"},{"type":"function","name":"ToolSearch"},{"type":"function","name":"Bash"},{"type":"function","name":"Agent"}],"tool_choice":"required"}`,
			check: func(t *testing.T, actual []byte) {
				declarations := mainRuntimeToolDeclarations(actual)
				for _, name := range []string{"WebSearch", "ToolSearch", "Bash", "Agent"} {
					if declarations[name].Get("type").String() != "function" {
						t.Fatalf("client function %s was replaced: %s", name, actual)
					}
				}
			},
		},
		{
			name: "json_object_and_explicit_store",
			body: `{"input":"JSON object result","text":{"format":{"type":"json_object"}},"store":true,"include":[],"parallel_tool_calls":false,"generate":true,"previous_response_id":null}`,
			check: func(t *testing.T, actual []byte) {
				if gjson.GetBytes(actual, "text.format.type").String() != "json_object" || gjson.GetBytes(actual, "store").Type != gjson.True || gjson.GetBytes(actual, "include").Raw != "[]" || gjson.GetBytes(actual, "parallel_tool_calls").Type != gjson.False {
					t.Fatalf("explicit model controls were replaced: %s", actual)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), testContext, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(test.body)}, runtimeTestOptions(), false)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, read(t))
		})
	}
	t.Run("final_payload_rules", func(t *testing.T) {
		models := []config.PayloadModelRule{{Name: "gpt-6.1-sol"}}
		cfg.Payload = config.PayloadConfig{
			Override: []config.PayloadRule{{Models: models, Params: map[string]any{"parallel_tool_calls": false, "include": []any{}, "tool_choice": "none", "temperature": 0.2}}},
			Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"tools", "client_metadata.optional_trace"}}},
		}
		defer func() { cfg.Payload = config.PayloadConfig{} }()
		body := []byte(`{"input":"payload rules","parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"tool_choice":"required","tools":[{"type":"function","name":"Read"}],"client_metadata":{"optional_trace":"remove"}}`)
		_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), testContext, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, runtimeTestOptions(), false)
		if err != nil {
			t.Fatal(err)
		}
		actual := read(t)
		if len(mainRuntimeToolDeclarations(actual)) != 0 || gjson.GetBytes(actual, "client_metadata.optional_trace").Exists() || gjson.GetBytes(actual, "parallel_tool_calls").Type != gjson.False || gjson.GetBytes(actual, "tool_choice").String() != "none" || gjson.GetBytes(actual, "include").Raw != "[]" || gjson.GetBytes(actual, "temperature").Float() != 0.2 {
			t.Fatalf("worker overwrote final CPA business rules: %s", actual)
		}
	})
	t.Run("client_tool_only_completion", func(t *testing.T) {
		body := []byte(`{"input":"main-client-tool-only","tools":[{"type":"custom","name":"run_source","format":{"type":"text"}}]}`)
		output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), testContext, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, runtimeTestOptions(), true)
		if err != nil || !bytes.Contains(output, []byte(`"call_id":"call_client"`)) || !bytes.Contains(output, []byte("response.completed")) {
			t.Fatalf("tool-only completion lost: %v %s", err, output)
		}
		read(t)
		if _, err := os.Stat(filepath.Join(home, "must-not-be-created")); !os.IsNotExist(err) {
			t.Fatalf("worker executed a client tool: %v", err)
		}
	})
	t.Run("live_client_delivery", func(t *testing.T) {
		manager := coreauth.NewManager(nil, nil, nil)
		manager.SetConfig(cfg)
		manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-6.1-sol"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		handler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), "cliproxy.roundtripper", http.DefaultTransport))
			c.Next()
		})
		router.POST("/v1/responses", handler.Responses)
		server := httptest.NewServer(router)
		defer server.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		transport := &http.Transport{}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","input":"main-live-stream","stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		type result struct {
			response *http.Response
			err      error
		}
		responseCh := make(chan result, 1)
		started := time.Now()
		go func() {
			response, err := client.Do(req)
			responseCh <- result{response, err}
		}()
		send := func(event []byte) {
			t.Helper()
			select {
			case phase <- event:
			case <-ctx.Done():
				t.Fatal("worker waited for model completion before delivering", ctx.Err())
			}
		}
		send([]byte(`{"type":"response.created","response":{"id":"resp_live","status":"in_progress"}}`))
		response := <-responseCh
		if response.err != nil {
			t.Fatal(response.err)
		}
		defer func() { _ = response.response.Body.Close() }()
		reader := bridge.NewSSEReader(response.response.Body)
		for index, kind := range []string{"response.created", "response.output_text.delta", "response.output_text.delta", "response.completed"} {
			if index == 1 || index == 2 {
				send([]byte(`{"type":"response.output_text.delta","item_id":"msg_live","output_index":0,"content_index":0,"delta":"increment"}`))
			} else if index == 3 {
				send([]byte(`{"type":"response.completed","response":{"id":"resp_live","status":"completed","output":[]}}`))
			}
			frame, err := reader.Next()
			if err != nil || gjson.GetBytes(frame.Data, "type").String() != kind {
				t.Fatalf("event %d not delivered before next phase: %s %v", index, frame.Data, err)
			}
			t.Logf("client event=%s elapsed=%s; upstream next phase was not released", kind, time.Since(started))
		}
		if _, err := io.ReadAll(response.response.Body); err != nil {
			t.Fatal(err)
		}
		read(t)
	})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_failure/stream=%t", stream), func(t *testing.T) {
			before := workerCalls.Load()
			_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), testContext, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":"main-upstream-failure"}`)}, runtimeTestOptions(), stream)
			if err == nil || !strings.Contains(err.Error(), "main-upstream-quota") || workerCalls.Load() != before+1 {
				t.Fatalf("upstream error or retry ownership changed: %v", err)
			}
			read(t)
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testContext)
		defer cancel()
		result, err := NewCodexAutoExecutor(cfg).ExecuteStream(ctx, auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"input":"main-cancel-request"}`)}, runtimeTestOptions())
		if err != nil {
			t.Fatal(err)
		}
		if first := <-result.Chunks; first.Err != nil {
			t.Fatal(first.Err)
		}
		cancel()
		for range result.Chunks {
		}
		select {
		case <-cancelled:
		case <-time.After(10 * time.Second):
			t.Fatal("worker model request survived cancellation")
		}
		read(t)
	})
	t.Run("native_lite_parallel_calls", func(t *testing.T) {
		testCodexRuntimeLiteParallelCalls(t, cfg, auth, read)
	})
	t.Run("native_history_field_replay", func(t *testing.T) {
		testCodexRuntimeNativeFieldReplay(t, cfg, auth, nil, read)
	})
	if cpaCalls.Load() != 0 {
		t.Fatalf("CPA directly sent %d model requests", cpaCalls.Load())
	}
	t.Logf("actual sender: CPA model HTTP=%d, worker model HTTP=%d", cpaCalls.Load(), workerCalls.Load())
}

func startCompatibleMainWorker(t *testing.T, binary, home, upstreamURL string) (*config.Config, *coreauth.Auth) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	cfg := &config.Config{AuthDir: filepath.Join(home, "auths"), Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, URL: fmt.Sprintf("ws://127.0.0.1:%d/", port)}}}
	auth := runtimeTestAuth("compatible-main.json")
	runtimeImageAuth(t, cfg, auth)
	auth.Attributes["base_url"] = upstreamURL + "/v1"
	metadata, _ := codexshared.Read(filepath.Join(cfg.AuthDir, auth.ID))
	metadata["id_token"] = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fake-user","email":"fixture@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"image-account","chatgpt_user_id":"fake-user","chatgpt_plan_type":"pro"}}`)) + ".signature"
	metadata["account_id"], metadata["refresh_token"], metadata["last_refresh"] = "image-account", "unused-synthetic-refresh", time.Now().UTC().Format(time.RFC3339)
	if err := codexshared.Write(filepath.Join(cfg.AuthDir, auth.ID), metadata); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	accountHome := filepath.Join(state, auth.ID)
	if err := os.MkdirAll(accountHome, 0700); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("model = \"gpt-6.1-sol\"\nmodel_provider = \"main_mock\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nchatgpt_base_url = %q\n[model_providers.main_mock]\nname = \"OpenAI\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", upstreamURL, upstreamURL+"/v1")
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), []byte(toml), 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(home, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + state, "RUST_LOG=error", "CODEX_CPA_AUTH_DIR=" + cfg.AuthDir, "CODEX_CPA_PORT=" + strconv.Itoa(port), "CODEX_APP_SERVER_MANAGED_CONFIG_PATH=" + filepath.Join(home, "managed.toml"), "CODEX_APP_SERVER_LOGIN_ISSUER=" + upstreamURL, "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + upstreamURL + "/oauth/token"}
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
			data, _ := os.ReadFile(logFile.Name())
			t.Logf("worker diagnostics: %s", data)
		}
	})
	startup := time.NewTimer(45 * time.Second)
	defer startup.Stop()
	ticks := time.NewTicker(20 * time.Millisecond)
	defer ticks.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("master exited: %v", err)
		case <-startup.C:
			t.Fatal("master did not become ready")
		case <-ticks.C:
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				_ = conn.Close()
				return cfg, auth
			}
		}
	}
}

func mainRuntimeToolDeclarations(body []byte) map[string]gjson.Result {
	declarations := map[string]gjson.Result{}
	var add func(gjson.Result)
	add = func(tools gjson.Result) {
		for _, tool := range tools.Array() {
			declarations[tool.Get("name").String()] = tool
			if tool.Get("type").String() == "namespace" && tool.Get("name").String() == "functions" {
				add(tool.Get("tools"))
			}
		}
	}
	add(gjson.GetBytes(body, "tools"))
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			add(item.Get("tools"))
		}
	}
	return declarations
}

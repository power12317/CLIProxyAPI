package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// TestCodexRuntimeForkContract runs the CPA executor against an actual fork binary.
// Its isolated HOME, synthetic OAuth grant, and HTTP mocks never use user credentials.
func TestCodexRuntimeForkContract(t *testing.T) {
	binary := os.Getenv("CODEX_RUNTIME_V1_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_V1_TEST_BINARY to the historical Unix v1 fork artifact")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("CODEX_RUNTIME_TEST_BINARY must be absolute")
	}
	dir, err := os.MkdirTemp("/tmp", "cpa-fork-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var inferenceCalls, acceptedCalls, refreshCalls atomic.Int32
	upstreamCancelled := make(chan struct{})
	captured := make(chan []byte, 8)
	idToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"synthetic@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_plan_type":"plus","chatgpt_user_id":"synthetic-user"}}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte("test-signature"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect || (r.URL.Host != "" && !strings.HasPrefix(r.URL.Host, "127.0.0.1:")) {
			http.Error(w, "external network blocked by test", 502)
			return
		}
		switch r.URL.Path {
		case "/v1/responses":
			inferenceCalls.Add(1)
			if r.Header.Get("Chatgpt-Account-Id") != "account" {
				t.Error("wrong account header")
			}
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Error(errRead)
				return
			}
			if r.Header.Get("Authorization") == "Bearer synthetic-access" {
				http.Error(w, "synthetic expired access token", 401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer synthetic-refreshed" {
				t.Error("unexpected managed access token")
				http.Error(w, "wrong token", 401)
				return
			}
			if gjson.GetBytes(body, "test_failure").Bool() {
				http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
				return
			}
			captured <- body
			acceptedCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Set-Cookie", "synthetic-secret")
			if gjson.GetBytes(body, "test_cancel").Bool() {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"cancel\"}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamCancelled)
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.future_extension\",\"extension\":true}\n\ndata: %s\n\n", runtimeTerminal)
		case "/oauth/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-refreshed", "refresh_token": "synthetic-rotated", "id_token": idToken})
		case "/backend-api/wham/accounts/check", "/api/codex/accounts/check":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accounts":[{"id":"account","workspace_backend_origin":"https://chatgpt.com","account_routing_override":"NO_CONSTRAINT"}]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
		}
	}))
	defer upstream.Close()
	authData, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": time.Now().UTC().Format(time.RFC3339), "tokens": map[string]any{"id_token": idToken, "access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "account_id": "account"}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), authData, 0600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`model = "runtime-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
cli_auth_credentials_store = "file"
chatgpt_base_url = %q
[model_providers.mock_provider]
name = "OpenAI"
base_url = %q
wire_api = "responses"
requires_openai_auth = true
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 10
[cpa_bridge]
enabled = true
credential_id = "fork"
`, upstream.URL, upstream.URL+"/v1")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "cpa.sock")
	logPath := filepath.Join(dir, "runtime.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--listen", "unix://"+socket)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + dir, "RUST_LOG=error", "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + upstream.URL + "/oauth/token", "HTTP_PROXY=" + upstream.URL, "HTTPS_PROXY=" + upstream.URL, "ALL_PROXY=" + upstream.URL, "NO_PROXY=127.0.0.1,localhost"}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-processDone
		_ = logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("synthetic runtime log: %s", data)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fork socket did not become ready")
		case <-ticker.C:
		}
	}
	w := config.CodexRuntimeWorker{ID: "fork", Socket: socket, AccountID: "account", Models: []string{"runtime-model"}}
	a := &coreauth.Auth{ID: "codex-runtime:fork", Provider: coreauth.CodexRuntimeProvider, Attributes: map[string]string{coreauth.AttributeCodexRuntimeID: "fork", "account_id": "account", "socket": socket}}
	e := runtimeTestExecutor(w)
	body := []byte(`{"model":"runtime-model","input":[],"instructions":"preserve instructions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"future_field":{"untouched":true}}`)
	response, err := e.Execute(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: body}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	equalRuntimeJSON(t, response.Payload, []byte(gjson.Get(runtimeTerminal, "response").Raw))
	got := <-captured
	for _, field := range []string{"input", "instructions", "tools", "tool_choice", "future_field"} {
		equalRuntimeJSON(t, []byte(gjson.GetBytes(body, field).Raw), []byte(gjson.GetBytes(got, field).Raw))
	}
	// Native unauthorized recovery may first reload storage, then refresh the grant.
	if refreshCalls.Load() != 1 || acceptedCalls.Load() != 1 || inferenceCalls.Load() > 3 {
		t.Fatalf("managed recovery counts: refresh=%d accepted=%d attempts=%d", refreshCalls.Load(), acceptedCalls.Load(), inferenceCalls.Load())
	}
	attemptsBeforeStream := inferenceCalls.Load()
	saved, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(saved, "tokens.refresh_token").String() != "synthetic-rotated" {
		t.Fatal("Codex did not own refresh persistence")
	}
	stream, err := e.ExecuteStream(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: body}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	var events int
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		events++
	}
	if events != 2 || acceptedCalls.Load() != 2 || inferenceCalls.Load() != attemptsBeforeStream+1 {
		t.Fatalf("stream/no automatic follow-up: events=%d calls=%d", events, inferenceCalls.Load())
	}
	beforeRejected := inferenceCalls.Load()
	_, err = e.Execute(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[],"client_metadata":{"unsupported":true}}`)}, runtimeTestOptions())
	if err == nil || inferenceCalls.Load() != beforeRejected {
		t.Fatal("unsupported identity metadata reached upstream")
	}
	_, err = e.Execute(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[],"test_failure":true}`)}, runtimeTestOptions())
	if err == nil || inferenceCalls.Load() != beforeRejected+1 {
		t.Fatalf("failed upstream request was retried: attempts=%d err=%v", inferenceCalls.Load()-beforeRejected, err)
	}
	cancelCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	cancelStream, err := e.ExecuteStream(cancelCtx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[],"test_cancel":true}`)}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if chunk := <-cancelStream.Chunks; chunk.Err != nil || len(chunk.Payload) == 0 {
		t.Fatal("cancel test did not start", chunk.Err)
	}
	cancelRequest()
	for range cancelStream.Chunks {
	}
	select {
	case <-upstreamCancelled:
	case <-ctx.Done():
		t.Fatal("upstream request survived cancellation")
	}
	client, err := bridge.Open(ctx, socket, "fork", "account")
	if err != nil {
		t.Fatal("runtime did not survive completed requests:", err)
	}
	client.Close()
}

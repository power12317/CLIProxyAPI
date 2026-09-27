package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// This optional test connects real CPA management/execution code to the fork using only fake OAuth.
func TestCodexRuntimeForkV2OAuthAndModeSwitch(t *testing.T) {
	binary := os.Getenv("CODEX_RUNTIME_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_TEST_BINARY to the v2 fork app-server")
	}
	home := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/cpa/v1/ws", port)
	h, sharedFile := runtimeHandler(t, false, endpoint)
	idToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fake-user","email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_user_id":"fake-user","chatgpt_plan_type":"plus"}}`)) + ".signature"
	var exchanges, refreshes, posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				var fields map[string]string
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
				}
				for k, v := range fields {
					r.Form.Set(k, v)
				}
			}
			access, refresh := "login-access", "login-refresh"
			if r.Form.Get("grant_type") == "authorization_code" {
				exchanges.Add(1)
				if r.Form.Get("code") != "fake-code" || r.Form.Get("code_verifier") == "" {
					t.Error("official code exchange missing PKCE/code")
				}
			} else {
				refreshes.Add(1)
				if r.Form.Get("refresh_token") != "login-refresh" {
					t.Errorf("unexpected mock token request: grant=%q refresh=%q", r.Form.Get("grant_type"), r.Form.Get("refresh_token"))
				}
				access, refresh = "refreshed-access", "refreshed-refresh"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh, "id_token": idToken, "expires_in": 3600})
		case "/v1/responses":
			posts.Add(1)
			w.Header().Set("X-Request-Id", "fork-upstream-request")
			w.Header().Set("X-Codex-Primary-Used-Percent", "12")
			if r.Header.Get("Authorization") == "Bearer login-access" {
				http.Error(w, "expired", 401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer refreshed-access" {
				t.Error("inference did not use current file token")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.future_extension\",\"future\":true}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-1\",\"status\":\"completed\",\"model\":\"runtime-model\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"lookup\",\"arguments\":\"{}\"}]}}\n\n")
		case "/backend-api/wham/accounts/check", "/api/codex/accounts/check":
			_, _ = io.WriteString(w, `{"accounts":[{"id":"account","workspace_backend_origin":"https://chatgpt.com","account_routing_override":"NO_CONSTRAINT"}]}`)
		default:
			_, _ = io.WriteString(w, `{"models":[]}`)
		}
	}))
	defer upstream.Close()
	toml := fmt.Sprintf(`model = "runtime-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
chatgpt_base_url = %q
[model_providers.mock_provider]
name = "OpenAI"
base_url = %q
wire_api = "responses"
requires_openai_auth = true
request_max_retries = 0
stream_max_retries = 0
`, upstream.URL, upstream.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(toml), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "runtime.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "RUST_LOG=error", "CODEX_CPA_AUTH_FILE=" + sharedFile, "CODEX_CPA_WORKER_ID=worker-a", "CODEX_CPA_BRIDGE_KEY=test-bridge-key", "CODEX_CPA_PORT=" + strconv.Itoa(port), "CODEX_APP_SERVER_LOGIN_ISSUER=" + upstream.URL, "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + upstream.URL + "/oauth/token"}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		_ = log.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Logf("fake runtime log: %s", raw)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("runtime not ready")
		case <-ticker.C:
		}
	}
	if rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":true}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	start := runtimeCall(t, h.StartCodexRuntimeLogin, "POST", "/", `{"worker_id":"worker-a"}`)
	if start.Code != 200 {
		t.Fatal(start.Body.String())
	}
	loginID := gjson.Get(start.Body.String(), "login_id").String()
	authURL, err := url.Parse(gjson.Get(start.Body.String(), "url").String())
	if err != nil {
		t.Fatal(err)
	}
	query := authURL.Query()
	if query.Get("code_challenge_method") != "S256" {
		t.Fatal("official PKCE missing")
	}
	callback := query.Get("redirect_uri") + "?code=fake-code&state=" + url.QueryEscape(query.Get("state"))
	payload, _ := json.Marshal(map[string]string{"worker_id": "worker-a", "login_id": loginID, "redirect_url": callback})
	completed := runtimeCall(t, h.CompleteCodexRuntimeLogin, "POST", "/", string(payload))
	if completed.Code != 200 || gjson.Get(completed.Body.String(), "status").String() != "completed" {
		t.Fatal(completed.Body.String())
	}
	if exchanges.Load() != 1 {
		t.Fatal("wrong code exchange count")
	}
	var auth *coreauth.Auth
	for _, a := range h.authManager.List() {
		if a.FileName == "worker-a.json" {
			auth = a
			break
		}
	}
	if auth == nil {
		t.Fatal("CPA did not load authorized shared file")
	}
	auth.Attributes["base_url"] = upstream.URL + "/v1"
	cfg := h.codexRuntimeConfig()
	cfg.RequestLog = true
	auto := executor.NewCodexAutoExecutor(cfg)
	req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":[]}`)}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	newLogContext := func() (context.Context, *gin.Context) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		return logging.WithResponseHeadersHolder(context.WithValue(ctx, "gin", c)), c
	}
	assertLogs := func(c *gin.Context, attempts int) {
		t.Helper()
		requestValue, _ := c.Get("API_REQUEST")
		responseValue, _ := c.Get("API_RESPONSE")
		requests, _ := requestValue.([]byte)
		responses, _ := responseValue.([]byte)
		if strings.Count(string(requests), "=== API REQUEST ") != attempts || !strings.Contains(string(requests), upstream.URL+"/v1/responses") {
			t.Fatalf("actual upstream request attempts missing: %s", requests)
		}
		for _, want := range []string{"Status: 200", "fork-upstream-request", "response.completed", "worker-a.json"} {
			if !strings.Contains(string(responses), want) {
				t.Errorf("upstream response log missing %q: %s", want, responses)
			}
		}
		if strings.Contains(string(requests), "login-access") || strings.Contains(string(requests), "refreshed-access") {
			t.Error("access token leaked into request logs")
		}
	}
	loggedCtx, ginCtx := newLogContext()
	if _, err := auto.Execute(loggedCtx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	assertLogs(ginCtx, 2)
	meta, err := codexshared.Read(sharedFile)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 || meta["refresh_token"] != "refreshed-refresh" {
		t.Fatal("Codex did not refresh common file")
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("unexpected independent auth.json")
	}
	if rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	before := posts.Load()
	loggedCtx, ginCtx = newLogContext()
	if _, err := auto.Execute(loggedCtx, auth, req, opts); err != nil {
		t.Fatal("CPA native mode:", err)
	}
	assertLogs(ginCtx, 1)
	if posts.Load() != before+1 {
		t.Fatal("native mode did not issue one request")
	}
	meta, _ = codexshared.Read(sharedFile)
	state, _ := codexshared.Get(meta)
	if state.Owner != "cpa" || !state.Enabled {
		t.Fatal("owner/preference mismatch")
	}
	if rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":true}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	loggedCtx, ginCtx = newLogContext()
	stream, err := auto.ExecuteStream(loggedCtx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var frames int
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		frames++
	}
	if frames != 2 {
		t.Fatal("raw events lost", frames)
	}
	assertLogs(ginCtx, 1)
	client, err := bridge.Dial(ctx, endpoint, "test-bridge-key")
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if strings.Contains(start.Body.String(), "login-refresh") {
		t.Fatal("login API exposed tokens")
	}
}

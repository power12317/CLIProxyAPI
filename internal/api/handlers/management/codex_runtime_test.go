package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func runtimeHandler(t *testing.T, enabled bool, endpoint string) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: enabled, Workers: []config.CodexRuntimeWorker{{ID: "worker-a", URL: endpoint, AuthFile: "worker-a.json", Token: "test-bridge-key"}}}}}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("codex: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := coreauth.NewManager(nil, nil, nil)
	h := NewHandler(cfg, configPath, m)
	return h, filepath.Join(dir, "worker-a.json")
}

func runtimeCall(t *testing.T, handler func(*gin.Context), method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return rec
}

func TestCodexRuntimeOffIsLocalAndPreferencesSurviveMasterToggle(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not connect", 500) }))
	defer server.Close()
	h, path := runtimeHandler(t, false, "ws"+strings.TrimPrefix(server.URL, "http")+"/cpa/v1/ws")
	meta := map[string]any{"type": "codex", "access_token": "from-codex", "refresh_token": "latest-refresh", "unknown": true}
	codexshared.Set(meta, codexshared.State{Enabled: true, WorkerID: "worker-a", Owner: "cpa"})
	if err := codexshared.Write(path, meta); err != nil {
		t.Fatal(err)
	}
	if rec := runtimeCall(t, h.GetCodexRuntime, "GET", "/codex-runtime", ""); rec.Code != 200 || strings.Contains(rec.Body.String(), "test-bridge-key") {
		t.Fatal(rec.Body.String())
	}
	for _, handler := range []func(*gin.Context){h.TestCodexRuntime, h.StartCodexRuntimeLogin, h.CompleteCodexRuntimeLogin} {
		if rec := runtimeCall(t, handler, "POST", "/", `{"worker_id":"worker-a"}`); rec.Code != 409 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if rec := runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?worker_id=worker-a&login_id=id", ""); rec.Code != 409 {
		t.Fatal(rec.Code)
	}
	for _, enabled := range []bool{true, false} {
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true}`
		}
		rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/codex-runtime", body)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		current, err := codexshared.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := codexshared.Get(current)
		want := "cpa"
		if enabled {
			want = "codex"
		}
		if state.Owner != want || !state.Enabled || current["access_token"] != "from-codex" || current["unknown"] != true {
			t.Fatal(current)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("config/status/disabled action contacted worker")
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil || loaded.Codex.Runtime.Enabled {
		t.Fatal("off switch not persisted", err)
	}
	if rec := runtimeCall(t, h.SetCodexRuntimeCredential, "POST", "/", `{"name":"worker-a.json","worker_id":"worker-a","enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
}

func TestCodexRuntimeWorkerUpdatePreservesOrClearsTokenAndRemovalKeepsCredential(t *testing.T) {
	h, path := runtimeHandler(t, true, "ws://127.0.0.1:38317/cpa/v1/ws")
	meta := map[string]any{"type": "codex", "access_token": "latest"}
	codexshared.Set(meta, codexshared.State{Enabled: true, WorkerID: "worker-a", Owner: "codex"})
	_ = codexshared.Write(path, meta)
	base := `{"workers":[{"id":"worker-a","url":"ws://127.0.0.1:38317/cpa/v1/ws","auth_file":"worker-a.json","models":[]%s}]}`
	for _, extra := range []string{"", `,"token":""`} {
		body := strings.Replace(base, "%s", extra, 1)
		rec := runtimeCall(t, h.PutCodexRuntime, "PUT", "/", body)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		if gjson.Get(rec.Body.String(), "workers.0.token_configured").Bool() != (extra == "") {
			t.Fatal("token semantics", rec.Body.String())
		}
	}
	ctx, release := bridge.Track(context.Background(), path)
	defer release()
	rec := runtimeCall(t, h.PutCodexRuntime, "PUT", "/", `{"workers":[]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("removed worker request not canceled")
	}
	current, _ := codexshared.Read(path)
	state, _ := codexshared.Get(current)
	if state.Owner != "cpa" || current["access_token"] != "latest" {
		t.Fatal(current)
	}
}

func TestCodexRuntimeManualOAuthUsesSeparateConnectionsAndSharedFile(t *testing.T) {
	var path string
	var starts, callbacks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-bridge-key" {
			t.Error("missing bridge authentication")
			w.WriteHeader(401)
			return
		}
		u := websocket.Upgrader{}
		conn, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params map[string]any  `json:"params"`
			}
			if conn.ReadJSON(&msg) != nil {
				return
			}
			var result any = map[string]any{}
			switch msg.Method {
			case "initialized":
				continue
			case "initialize":
			case "cpa/capabilities/read", "cpa/credential/reload":
				result = bridge.Capabilities{ProtocolVersion: 2, CredentialID: "worker-a", CredentialFile: "worker-a.json", ExecutionMode: "inference-only", RawEvents: true, AuthOwner: "codex", ManualOAuth: true}
			case "cpa/auth/login/start":
				starts.Add(1)
				metadata, err := codexshared.Read(path)
				if err != nil {
					t.Error(err)
					return
				}
				state, _ := codexshared.Get(metadata)
				if state.Owner != "codex" {
					t.Error("login file not prepared")
				}
				result = map[string]any{"loginId": "login-1", "authUrl": "https://auth.example/authorize?state=state-1", "state": "state-1"}
			case "cpa/auth/login/callback":
				callbacks.Add(1)
				if msg.Params["loginId"] != "login-1" || msg.Params["redirectUrl"] != "http://localhost:1455/auth/callback?state=state-1&code=synthetic" {
					t.Error("callback changed")
				}
				metadata, _ := codexshared.Read(path)
				metadata["access_token"] = "worker-token"
				metadata["refresh_token"] = "worker-refresh"
				metadata["account_id"] = "account"
				_ = codexshared.Write(path, metadata)
				result = map[string]any{"status": "completed", "error": nil}
			case "cpa/auth/login/status":
				result = map[string]any{"status": "completed", "error": nil}
			default:
				t.Errorf("unexpected RPC %s", msg.Method)
				return
			}
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": result})
		}
	}))
	defer server.Close()
	h, authPath := runtimeHandler(t, true, "ws"+strings.TrimPrefix(server.URL, "http")+"/cpa/v1/ws")
	path = authPath
	rec := runtimeCall(t, h.StartCodexRuntimeLogin, "POST", "/", `{"worker_id":"worker-a"}`)
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "login_id").String() != "login-1" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = runtimeCall(t, h.CompleteCodexRuntimeLogin, "POST", "/", `{"worker_id":"worker-a","login_id":"login-1","redirect_url":"http://localhost:1455/auth/callback?state=state-1&code=synthetic"}`)
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "status").String() != "completed" {
		t.Fatal(rec.Body.String())
	}
	rec = runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?worker_id=worker-a&login_id=login-1", "")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	meta, _ := codexshared.Read(path)
	if meta["refresh_token"] != "worker-refresh" || starts.Load() != 1 || callbacks.Load() != 1 {
		t.Fatal("OAuth flow did not persist shared credential")
	}
	if rec = runtimeCall(t, h.GetCodexRuntime, "GET", "/", ""); strings.Contains(rec.Body.String(), "worker-token") || strings.Contains(rec.Body.String(), "worker-refresh") {
		t.Fatal("status returned OAuth secrets")
	}
}

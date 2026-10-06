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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func runtimeHandler(t *testing.T, enabled bool, endpoint string) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: filepath.Join(dir, "auths"), Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: enabled, URL: endpoint}}}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("codex: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandler(cfg, configPath, manager)
	return h, filepath.Join(cfg.AuthDir, "existing-account.json")
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

func runtimeJSON(t *testing.T, body any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeRuntimeCredential(t *testing.T, path string, enabled bool, extra map[string]any) {
	t.Helper()
	metadata := map[string]any{"type": "codex", "access_token": "existing-access", "refresh_token": "existing-refresh", "unknown": "retained"}
	for key, value := range extra {
		metadata[key] = value
	}
	codexshared.Set(metadata, codexshared.State{Enabled: enabled})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeEnabled(t *testing.T, path string, enabled bool) map[string]any {
	t.Helper()
	metadata, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	state, exists := codexshared.Get(metadata)
	if !exists || state.Enabled != enabled {
		t.Fatalf("unexpected effective flag in %s: %v", path, metadata)
	}
	fields := metadata["codex_cli"].(map[string]any)
	if _, exists := fields["worker_id"]; exists {
		t.Fatal("worker ID persisted", fields)
	}
	if _, exists := fields["owner"]; exists {
		t.Fatal("owner persisted", fields)
	}
	return metadata
}

func runtimeMaster(t *testing.T, handle func(string, map[string]any) any) (string, *atomic.Int32) {
	t.Helper()
	connections := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected bridge authentication")
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
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
			case "cpa/capabilities/read":
				result = bridge.Capabilities{ProtocolVersion: 3, ExecutionMode: "inference-only", RawBody: true, ManualOAuth: true, UpstreamLogs: true, UpstreamBodyLogs: true, Operations: []string{"responses"}}
			default:
				result = handle(msg.Method, msg.Params)
			}
			if failure, ok := result.(*bridge.Error); ok {
				if err := conn.WriteJSON(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": failure.Message, "data": map[string]any{"httpStatus": failure.Status}}}); err != nil {
					return
				}
				continue
			}
			if err := conn.WriteJSON(map[string]any{"id": msg.ID, "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/cpa/v1/ws", connections
}

func TestCodexRuntimeOffRemainsLocalAndHidesTopology(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "must not connect", 500)
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/cpa/v1/ws"
	h, path := runtimeHandler(t, false, endpoint)
	writeRuntimeCredential(t, path, false, map[string]any{"email": "existing@example.invalid"})
	if err := bridge.ReconcileState(h.codexRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	rec := runtimeCall(t, h.GetCodexRuntime, "GET", "/", "")
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "credentials.0.label").String() != "existing@example.invalid" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, field := range []string{"workers", "worker_id", "url", "token", "auth_file", "models", "credential_modes", "access_token", "refresh_token"} {
		if strings.Contains(rec.Body.String(), `"`+field+`"`) {
			t.Errorf("internal field exposed: %s", field)
		}
	}
	state := "runtime-off-standard-session"
	RegisterOAuthSessionWithMetadata(state, "codex", map[string]any{"runtime_login_id": "off-login", "credential_id": "existing-account.json", "redirect_uri": "http://localhost:1455/auth/callback"})
	t.Cleanup(func() { CancelOAuthSession(state) })
	if result := runtimeCall(t, h.PostOAuthCallback, "POST", "/", `{"provider":"codex","state":"runtime-off-standard-session","code":"unused"}`); result.Code != 409 {
		t.Fatal(result.Code, result.Body.String())
	}
	if result := runtimeCall(t, h.GetAuthStatus, "GET", "/?state="+state, ""); result.Code != 200 || gjson.Get(result.Body.String(), "status").String() != "error" {
		t.Fatal(result.Code, result.Body.String())
	}
	if result := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":false,"url":"ws://other","workers":[]}`); result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	if result := runtimeCall(t, h.SetCodexRuntimeCredential, "POST", "/", `{"name":"existing-account.json","enabled":false}`); result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("disabled operations contacted master")
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil || loaded.Codex.Runtime.Enabled || loaded.Codex.Runtime.URL != endpoint || loaded.Codex.Runtime.CredentialModes["existing-account.json"] {
		t.Fatal("config preferences or internal endpoint changed", loaded, err)
	}
	metadata := assertRuntimeEnabled(t, path, false)
	if metadata["access_token"] != "existing-access" || metadata["unknown"] != "retained" {
		t.Fatal("shared credential values changed", metadata)
	}
}

func TestCodexRuntimePreferencesSurviveGlobalToggleAndReloadCPA(t *testing.T) {
	var reloads atomic.Int32
	endpoint, _ := runtimeMaster(t, func(method string, _ map[string]any) any {
		if method != "cpa/credential/reload" {
			t.Errorf("unexpected RPC %s", method)
		}
		reloads.Add(1)
		return bridge.Capabilities{ProtocolVersion: 3, RawBody: true, ExecutionMode: "inference-only"}
	})
	h, path := runtimeHandler(t, false, endpoint)
	other := bridge.CredentialPath(h.cfg, "组别/账号 B.json")
	writeRuntimeCredential(t, path, false, nil)
	writeRuntimeCredential(t, other, false, nil)
	var cpaReloads int
	h.SetConfigReloadHook(func(_ context.Context, snapshot *config.Config) {
		cpaReloads++
		if snapshot.Codex.Runtime.URL != endpoint {
			t.Error("CPA reload lost master endpoint")
		}
	})
	if err := bridge.ReconcileState(h.codexRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	body := runtimeJSON(t, map[string]any{"name": "组别/账号 B.json", "enabled": false})
	if rec := runtimeCall(t, h.SetCodexRuntimeCredential, "POST", "/", body); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, enabled := range []bool{true, false, true} {
		rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", runtimeJSON(t, map[string]any{"enabled": enabled}))
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		assertRuntimeEnabled(t, path, enabled)
		assertRuntimeEnabled(t, other, false)
		rows := gjson.Get(rec.Body.String(), "credentials").Array()
		for _, row := range rows {
			if row.Get("enabled").Bool() != (row.Get("name").String() == "existing-account.json") {
				t.Fatal("global toggle lost per-account preference", rec.Body.String())
			}
		}
	}
	if reloads.Load() != 3 || cpaReloads != 4 {
		t.Fatalf("master reloads=%d, CPA reloads=%d", reloads.Load(), cpaReloads)
	}
	registered, exists := h.authManager.GetByID("组别/账号 B.json")
	if !exists || registered.ID != "组别/账号 B.json" {
		t.Fatal("relative credential identity changed in auth manager", registered)
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil || loaded.Codex.Runtime.CredentialModes["组别/账号 B.json"] || !loaded.Codex.Runtime.CredentialModes["existing-account.json"] {
		t.Fatal("per-account preferences were not saved", err)
	}
}

func TestCodexRuntimeRetainsFileOptOutWhenDisablingGlobalMode(t *testing.T) {
	endpoint, _ := runtimeMaster(t, func(string, map[string]any) any { return map[string]any{} })
	h, path := runtimeHandler(t, true, endpoint)
	h.cfg.Codex.Runtime.CredentialModes = map[string]bool{"existing-account.json": true}
	writeRuntimeCredential(t, path, true, nil)
	if err := bridge.ReconcileState(h.codexRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	// A direct file edit overrides the earlier selection before the global stop.
	writeRuntimeCredential(t, path, false, nil)
	for _, enabled := range []bool{false, true} {
		rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", runtimeJSON(t, map[string]any{"enabled": enabled}))
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		assertRuntimeEnabled(t, path, false)
	}
	if selected, exists := h.codexRuntimeConfig().Codex.Runtime.CredentialModes["existing-account.json"]; !exists || selected {
		t.Fatal("file opt-out not preserved in configuration")
	}
}

func TestCodexRuntimeInvalidSelectionDoesNotCreateOrContactMaster(t *testing.T) {
	endpoint, connections := runtimeMaster(t, func(string, map[string]any) any {
		t.Error("invalid selection contacted master")
		return map[string]any{}
	})
	h, path := runtimeHandler(t, true, endpoint)
	writeRuntimeCredential(t, path, true, nil)
	for _, name := range []string{"not-found.json", "EXISTING-account.json", "../external.json"} {
		body := runtimeJSON(t, map[string]any{"name": name, "enabled": true})
		if rec := runtimeCall(t, h.SetCodexRuntimeCredential, "POST", "/", body); rec.Code != 400 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if connections.Load() != 0 {
		t.Fatal("invalid selection connected to master")
	}
	files, err := bridge.ListCredentials(h.cfg)
	if err != nil || len(files) != 1 {
		t.Fatal("invalid selection created or renamed credentials", files, err)
	}
}

func TestCodexRuntimeOffStillReloadsCPAWhenMasterUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unavailable", 503) }))
	defer server.Close()
	h, path := runtimeHandler(t, true, "ws"+strings.TrimPrefix(server.URL, "http"))
	writeRuntimeCredential(t, path, true, nil)
	if err := bridge.ReconcileState(h.codexRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	called := false
	h.SetConfigReloadHook(func(_ context.Context, snapshot *config.Config) {
		called = true
		if snapshot.Codex.Runtime.Enabled {
			t.Error("CPA reload received stale enabled setting")
		}
	})
	rec := runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":false}`)
	if rec.Code != 503 || !called || h.codexRuntimeConfig().Codex.Runtime.Enabled {
		t.Fatal("master error prevented local CPA takeover", rec.Code, rec.Body.String())
	}
	assertRuntimeEnabled(t, path, false)
	auth, exists := h.authManager.GetByID("existing-account.json")
	state, _ := codexshared.Get(auth.Metadata)
	if !exists || state.Enabled {
		t.Fatal("auth manager retained worker ownership", auth)
	}
}

func TestCodexRuntimePanelCanReenableAFileDisabledOutsideCPA(t *testing.T) {
	var reloads atomic.Int32
	endpoint, _ := runtimeMaster(t, func(method string, _ map[string]any) any {
		if method != "cpa/credential/reload" {
			t.Errorf("unexpected RPC %s", method)
		}
		reloads.Add(1)
		return map[string]any{}
	})
	h, path := runtimeHandler(t, true, endpoint)
	h.cfg.Codex.Runtime.CredentialModes = map[string]bool{"existing-account.json": true}
	writeRuntimeCredential(t, path, true, nil)
	if err := bridge.ReconcileState(h.codexRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	writeRuntimeCredential(t, path, false, nil)
	rec := runtimeCall(t, h.GetCodexRuntime, "GET", "/", "")
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "credentials.0.enabled").Bool() || gjson.Get(rec.Body.String(), "credentials.0.owner").String() != "cpa" {
		t.Fatal("status ignored the effective file selection", rec.Code, rec.Body.String())
	}
	if reloads.Load() != 0 {
		t.Fatal("status contacted master")
	}
	rec = runtimeCall(t, h.SetCodexRuntimeCredential, "POST", "/", `{"name":"existing-account.json","enabled":true}`)
	if rec.Code != 200 || !gjson.Get(rec.Body.String(), "credentials.0.enabled").Bool() || gjson.Get(rec.Body.String(), "credentials.0.owner").String() != "codex" {
		t.Fatal("explicit enable did not apply unchanged configuration preference", rec.Code, rec.Body.String())
	}
	assertRuntimeEnabled(t, path, true)
	if reloads.Load() != 1 {
		t.Fatalf("explicit enable reloaded master %d times", reloads.Load())
	}
}

package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	for _, handler := range []func(*gin.Context){h.StartCodexRuntimeLogin, h.CompleteCodexRuntimeLogin} {
		if result := runtimeCall(t, handler, "POST", "/", `{}`); result.Code != 409 {
			t.Fatal(result.Code, result.Body.String())
		}
	}
	if result := runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?login_id=id", ""); result.Code != 409 {
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

func TestCodexRuntimeOAuthUsesOriginalCredentialIDAndSharedFile(t *testing.T) {
	credentialID := "组别/Existing 账号.json"
	var path string
	var starts, callbacks, reloads atomic.Int32
	var boundID string
	endpoint, connections := runtimeMaster(t, func(method string, params map[string]any) any {
		switch method {
		case "cpa/credential/reload":
			reloads.Add(1)
			if id, exists := params["credentialId"]; exists && id != credentialID {
				t.Error("reload changed original credential ID", params)
			}
			return map[string]any{}
		case "cpa/auth/login/start":
			starts.Add(1)
			if params["credentialId"] != credentialID || len(params) != 1 {
				t.Error("start did not use only the CPA credential ID", params)
			}
			boundID = params["credentialId"].(string)
			metadata, err := codexshared.Read(path)
			state, _ := codexshared.Get(metadata)
			if err != nil || !state.Enabled || reloads.Load() == 0 {
				t.Error("login started before enabling and reloading credential", metadata, err)
			}
			return map[string]any{"loginId": "existing-login", "authUrl": "https://auth.example/authorize?state=state-1", "state": "state-1"}
		case "cpa/auth/login/callback":
			callbacks.Add(1)
			if len(params) != 2 || params["loginId"] != "existing-login" || params["redirectUrl"] != "http://localhost:1455/auth/callback?state=state-1&code=synthetic" {
				t.Error("callback routing changed", params)
			}
			metadata, err := codexshared.Read(path)
			if err != nil {
				t.Error(err)
			}
			metadata["access_token"], metadata["refresh_token"] = "worker-token", "worker-refresh"
			if err := codexshared.Write(path, metadata); err != nil {
				t.Error(err)
			}
			return map[string]any{"status": "completed", "error": nil}
		case "cpa/auth/login/status":
			if len(params) != 1 || params["loginId"] != "existing-login" {
				t.Error("status must route by login ID only", params)
			}
			return map[string]any{"status": "completed", "error": nil}
		default:
			t.Errorf("unexpected RPC %s", method)
			return map[string]any{}
		}
	})
	h, _ := runtimeHandler(t, true, endpoint)
	path = bridge.CredentialPath(h.cfg, credentialID)
	writeRuntimeCredential(t, path, false, nil)
	rec := runtimeCall(t, h.StartCodexRuntimeLogin, "POST", "/", runtimeJSON(t, map[string]any{"name": credentialID}))
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "login_id").String() != "existing-login" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	_, _, _, metadata, _, exists := GetOAuthSessionDetails("codex-runtime:existing-login")
	if !exists || metadata["credential_id"] != credentialID || len(metadata) != 1 || boundID != credentialID {
		t.Fatal("OAuth metadata uses a separate worker identity", metadata)
	}
	rec = runtimeCall(t, h.CompleteCodexRuntimeLogin, "POST", "/", `{"login_id":"existing-login","redirect_url":"http://localhost:1455/auth/callback?state=state-1&code=synthetic"}`)
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "status").String() != "completed" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?login_id=existing-login", "")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	current := assertRuntimeEnabled(t, path, true)
	if current["refresh_token"] != "worker-refresh" || current["unknown"] != "retained" || starts.Load() != 1 || callbacks.Load() != 1 {
		t.Fatal("OAuth changed shared-file identity or metadata", current)
	}
	auth, exists := h.authManager.GetByID(credentialID)
	if !exists || auth.Metadata["access_token"] != "worker-token" {
		t.Fatal("successful OAuth did not reload original auth record", auth)
	}
	files, err := bridge.ListCredentials(h.codexRuntimeConfig())
	if err != nil || len(files) != 1 || files[0].ID != credentialID {
		t.Fatal("existing credential renamed or copied", files, err)
	}
	if rec = runtimeCall(t, h.PutCodexRuntime, "PATCH", "/", `{"enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	callsAfterOff := connections.Load()
	if rec = runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?login_id=existing-login", ""); rec.Code != 409 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec = runtimeCall(t, h.CompleteCodexRuntimeLogin, "POST", "/", `{"login_id":"existing-login","redirect_url":"unused"}`); rec.Code != 409 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if connections.Load() != callsAfterOff {
		t.Fatal("disabled OAuth status contacted master")
	}
}

func TestCodexRuntimeNewOAuthCreatesOnlyOneNewUUIDCredential(t *testing.T) {
	var h *Handler
	var credentialID string
	endpoint, _ := runtimeMaster(t, func(method string, params map[string]any) any {
		if method == "cpa/auth/login/start" {
			credentialID, _ = params["credentialId"].(string)
			if _, err := uuid.Parse(strings.TrimSuffix(credentialID, ".json")); err != nil {
				t.Error("new credential is not UUID based", credentialID)
			}
			metadata, err := codexshared.Read(bridge.CredentialPath(h.cfg, credentialID))
			state, _ := codexshared.Get(metadata)
			if err != nil || !state.Enabled || metadata["type"] != "codex" {
				t.Error("new OAuth placeholder not ready", metadata, err)
			}
			return map[string]any{"loginId": "new-login", "authUrl": "https://auth.example/authorize", "state": "new-state"}
		}
		if method != "cpa/credential/reload" {
			t.Errorf("unexpected RPC %s", method)
		}
		return map[string]any{}
	})
	var existing string
	h, existing = runtimeHandler(t, true, endpoint)
	writeRuntimeCredential(t, existing, true, nil)
	if rec := runtimeCall(t, h.GetCodexRuntime, "GET", "/", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	files, err := bridge.ListCredentials(h.cfg)
	if err != nil || len(files) != 1 {
		t.Fatal("status created placeholder", files, err)
	}
	rec := runtimeCall(t, h.StartCodexRuntimeLogin, "POST", "/", `{}`)
	if rec.Code != 200 || credentialID == "" || credentialID == "existing-account.json" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	files, err = bridge.ListCredentials(h.cfg)
	if err != nil || len(files) != 2 {
		t.Fatal("new OAuth did not add exactly one credential", files, err)
	}
	current := assertRuntimeEnabled(t, existing, true)
	if current["access_token"] != "existing-access" || current["refresh_token"] != "existing-refresh" {
		t.Fatal("new OAuth modified an existing account")
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
		if rec := runtimeCall(t, h.StartCodexRuntimeLogin, "POST", "/", body); rec.Code != 400 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if rec := runtimeCall(t, h.GetCodexRuntimeLoginStatus, "GET", "/?login_id="+url.QueryEscape("unknown-login"), ""); rec.Code != 400 {
		t.Fatal(rec.Code, rec.Body.String())
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

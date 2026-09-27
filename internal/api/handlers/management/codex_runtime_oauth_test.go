package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type runtimeOAuthCall struct {
	method string
	params map[string]any
}

type runtimeOAuthMock struct {
	h           *Handler
	state       string
	loginID     string
	redirectURI string
	connections *atomic.Int32
	mu          sync.Mutex
	credential  string
	status      string
	failure     string
	calls       []runtimeOAuthCall
}

func newRuntimeOAuthMock(t *testing.T) *runtimeOAuthMock {
	t.Helper()
	h, _ := runtimeHandler(t, true, "")
	mock := &runtimeOAuthMock{
		h: h, state: uuid.NewString(), loginID: uuid.NewString(), status: "pending",
		redirectURI: "http://localhost:1455/auth/callback?existing=value",
	}
	endpoint, connections := runtimeMaster(t, func(method string, params map[string]any) any {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		mock.calls = append(mock.calls, runtimeOAuthCall{method: method, params: params})
		if mock.failure == method {
			return &bridge.Error{Status: http.StatusBadGateway, Message: "mock OAuth upstream failed"}
		}
		switch method {
		case "cpa/credential/reload":
			return map[string]any{}
		case "cpa/auth/login/start":
			mock.credential, _ = params["credentialId"].(string)
			if len(params) != 1 || mock.credential == "" {
				t.Error("OAuth start must only pass credential identity", params)
			}
			metadata, err := codexshared.Read(bridge.CredentialPath(h.codexRuntimeConfig(), mock.credential))
			state, _ := codexshared.Get(metadata)
			if err != nil || !state.Enabled {
				t.Error("OAuth started without an enabled shared credential", metadata, err)
			}
			return map[string]any{"loginId": mock.loginID, "state": mock.state, "authUrl": "https://auth.example/authorize?state=" + mock.state + "&redirect_uri=" + url.QueryEscape(mock.redirectURI)}
		case "cpa/auth/login/callback":
			if params["loginId"] != mock.loginID || len(params) != 2 {
				t.Error("callback must route by internal login ID", params)
			}
			path := bridge.CredentialPath(h.codexRuntimeConfig(), mock.credential)
			metadata, err := codexshared.Read(path)
			if err != nil {
				t.Error(err)
			}
			metadata["access_token"], metadata["refresh_token"] = "runtime-access", "runtime-refresh"
			if err := codexshared.Write(path, metadata); err != nil {
				t.Error(err)
			}
			mock.status = "completed"
			return map[string]any{"status": "completed", "error": nil}
		case "cpa/auth/login/status":
			if params["loginId"] != mock.loginID || len(params) != 1 {
				t.Error("status must route by internal login ID only", params)
			}
			message := ""
			if mock.status == "error" {
				message = "OAuth denied by upstream"
			}
			return map[string]any{"status": mock.status, "error": message}
		default:
			t.Errorf("unexpected RPC %s", method)
			return map[string]any{}
		}
	})
	mock.connections = connections
	h.cfg.Codex.Runtime.URL = endpoint
	t.Cleanup(func() { CancelOAuthSession(mock.state) })
	return mock
}

func (mock *runtimeOAuthMock) startedCredential() string {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return mock.credential
}

func (mock *runtimeOAuthMock) setStatus(status string) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.status = status
}

func (mock *runtimeOAuthMock) fail(method string) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.failure = method
}

func (mock *runtimeOAuthMock) lastCallback() string {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for i := len(mock.calls) - 1; i >= 0; i-- {
		if mock.calls[i].method == "cpa/auth/login/callback" {
			value, _ := mock.calls[i].params["redirectUrl"].(string)
			return value
		}
	}
	return ""
}

func (mock *runtimeOAuthMock) start(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := runtimeCall(t, mock.h.RequestCodexToken, http.MethodGet, "/codex-auth-url"+query, "")
	if rec.Code != http.StatusOK || gjson.Get(rec.Body.String(), "status").String() != "ok" || gjson.Get(rec.Body.String(), "state").String() != mock.state {
		t.Fatal("standard OAuth start failed", rec.Code, rec.Body.String())
	}
	if gjson.Get(rec.Body.String(), "login_id").Exists() {
		t.Fatal("internal login identity exposed to the OAuth page", rec.Body.String())
	}
	return rec
}

func assertNoRuntimeCallbackFile(t *testing.T, cfg *config.Config) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(cfg.AuthDir, ".oauth*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("runtime OAuth wrote local callback state", entries, err)
	}
}

func TestCodexRuntimeStandardOAuthReusesOriginalCredentialAndClientSystem(t *testing.T) {
	mock := newRuntimeOAuthMock(t)
	credentialID := "组别/Existing 账号.json"
	path := bridge.CredentialPath(mock.h.cfg, credentialID)
	writeRuntimeCredential(t, path, false, map[string]any{"codex_client_system": "windows", "label": "original label", "client_metadata": "preserved"})
	mock.h.syncRuntimeAuth(context.Background(), path)
	auth, found := mock.h.authManager.GetByID(credentialID)
	if !found {
		t.Fatal("original auth identity was not loaded")
	}
	auth.EnsureIndex()
	rec := mock.start(t, "?auth_index="+url.QueryEscape(auth.Index)+"&client_system=mac")
	if gjson.Get(rec.Body.String(), "client_system").String() != "windows" || mock.startedCredential() != credentialID {
		t.Fatal("targeted authorization changed existing identity/system", rec.Body.String())
	}
	provider, _, isPlugin, metadata, completed, exists := GetOAuthSessionDetails(mock.state)
	if !exists || isPlugin || completed || provider != "codex" || metadata["credential_id"] != credentialID || metadata["runtime_login_id"] != mock.loginID || metadata["redirect_uri"] != mock.redirectURI {
		t.Fatal("runtime session does not use original OAuth state", provider, metadata)
	}
	rec = runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
	if rec.Code != 200 || gjson.Get(rec.Body.String(), "status").String() != "wait" {
		t.Fatal("pending did not use standard wait status", rec.Code, rec.Body.String())
	}
	redirect := "http://localhost:1455/auth/callback?code=code-1&state=" + mock.state + "&other=untouched"
	rec = runtimeCall(t, mock.h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"provider": "codex", "redirect_url": redirect}))
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` || mock.lastCallback() != redirect {
		t.Fatal("callback did not preserve the standard API/full URL", rec.Code, rec.Body.String())
	}
	callsAfterCallback := mock.connections.Load()
	rec = runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` || mock.connections.Load() != callsAfterCallback {
		t.Fatal("completed session did not return cached standard status", rec.Code, rec.Body.String())
	}
	current := assertRuntimeEnabled(t, path, true)
	if current["access_token"] != "runtime-access" || current["refresh_token"] != "runtime-refresh" || current["codex_client_system"] != "windows" || current["client_metadata"] != "preserved" || current["label"] != "original label" {
		t.Fatal("targeted OAuth failed to preserve the existing file metadata", current)
	}
	registered, found := mock.h.authManager.GetByID(credentialID)
	if !found || registered.Metadata["access_token"] != "runtime-access" {
		t.Fatal("callback did not sync the original auth record", registered)
	}
	files, err := bridge.ListCredentials(mock.h.cfg)
	if err != nil || len(files) != 1 || files[0].ID != credentialID {
		t.Fatal("targeted OAuth renamed or copied the credential", files, err)
	}
	assertNoRuntimeCallbackFile(t, mock.h.cfg)
	if rec = runtimeCall(t, mock.h.PutCodexRuntime, http.MethodPatch, "/codex-runtime", `{"enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	callsAfterOff := mock.connections.Load()
	rec = runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` || mock.connections.Load() != callsAfterOff {
		t.Fatal("completed session changed after disabling runtime", rec.Code, rec.Body.String())
	}
}

func TestCodexRuntimeStandardOAuthCreatesNewFileOnlyAtStart(t *testing.T) {
	mock := newRuntimeOAuthMock(t)
	if rec := runtimeCall(t, mock.h.GetCodexRuntime, http.MethodGet, "/codex-runtime", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	files, err := bridge.ListCredentials(mock.h.cfg)
	if err != nil || len(files) != 0 {
		t.Fatal("status created credentials", files, err)
	}
	rec := mock.start(t, "?client_system=windows")
	if gjson.Get(rec.Body.String(), "client_system").String() != "windows" {
		t.Fatal("standard client system field lost", rec.Body.String())
	}
	credentialID := mock.startedCredential()
	if _, err := uuid.Parse(strings.TrimSuffix(credentialID, ".json")); err != nil {
		t.Fatal("new credential is not a UUID file", credentialID)
	}
	metadata := assertRuntimeEnabled(t, bridge.CredentialPath(mock.h.cfg, credentialID), true)
	if metadata["codex_client_system"] != "windows" || metadata["auth_kind"] != coreauth.AuthKindOAuth {
		t.Fatal("new credential lost selected system metadata", metadata)
	}
	files, err = bridge.ListCredentials(mock.h.cfg)
	if err != nil || len(files) != 1 || files[0].ID != credentialID {
		t.Fatal("new login did not create exactly one credential", files, err)
	}
}

func TestCodexRuntimeStandardCallbackReconstructsOfficialRedirect(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			mock := newRuntimeOAuthMock(t)
			mock.start(t, "")
			var rec *httptest.ResponseRecorder
			if method == http.MethodPost {
				rec = runtimeCall(t, mock.h.PostOAuthCallback, method, "/oauth-callback", runtimeJSON(t, map[string]any{"provider": "openai", "state": mock.state, "code": "code + 中文"}))
			} else {
				rec = runtimeCall(t, mock.h.GetOAuthCallback, method, "/oauth-callback?state="+mock.state+"&code="+url.QueryEscape("code + 中文"), "")
			}
			if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` {
				t.Fatal(rec.Code, rec.Body.String())
			}
			redirect, err := url.Parse(mock.lastCallback())
			if err != nil || redirect.Scheme != "http" || redirect.Host != "localhost:1455" || redirect.Path != "/auth/callback" || redirect.Query().Get("state") != mock.state || redirect.Query().Get("code") != "code + 中文" || redirect.Query().Get("existing") != "value" {
				t.Fatal("callback was not reconstructed from the official redirect URI", mock.lastCallback(), err)
			}
			assertNoRuntimeCallbackFile(t, mock.h.cfg)
		})
	}
}

func TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile(t *testing.T) {
	for _, status := range []string{"pending", "completed", "error"} {
		t.Run(status, func(t *testing.T) {
			mock := newRuntimeOAuthMock(t)
			mock.start(t, "")
			path := bridge.CredentialPath(mock.h.cfg, mock.startedCredential())
			metadata, err := codexshared.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			metadata["access_token"] = "polled-token"
			if err := codexshared.Write(path, metadata); err != nil {
				t.Fatal(err)
			}
			mock.setStatus(status)
			rec := runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
			want := map[string]string{"pending": "wait", "completed": "ok", "error": "error"}[status]
			if rec.Code != 200 || gjson.Get(rec.Body.String(), "status").String() != want {
				t.Fatal(rec.Code, rec.Body.String())
			}
			if status == "completed" {
				auth, found := mock.h.authManager.GetByID(mock.startedCredential())
				if !found || auth.Metadata["access_token"] != "polled-token" {
					t.Fatal("poll completion did not sync original file", auth)
				}
			}
			if status == "error" && gjson.Get(rec.Body.String(), "error").String() != "OAuth denied by upstream" {
				t.Fatal("poll error detail lost", rec.Body.String())
			}
		})
	}
}

func TestCodexRuntimeStandardOAuthHonorsExistingValidationAndCancellation(t *testing.T) {
	mock := newRuntimeOAuthMock(t)
	for _, index := range []string{"unknown", "../unknown"} {
		rec := runtimeCall(t, mock.h.RequestCodexToken, http.MethodGet, "/codex-auth-url?auth_index="+url.QueryEscape(index), "")
		if rec.Code != http.StatusNotFound {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	other := &coreauth.Auth{ID: "claude.json", Provider: "claude", Metadata: map[string]any{"type": "claude"}}
	other.EnsureIndex()
	if _, err := mock.h.authManager.Register(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	rec := runtimeCall(t, mock.h.RequestCodexToken, http.MethodGet, "/codex-auth-url?auth_index="+other.Index, "")
	if rec.Code != 400 || mock.connections.Load() != 0 {
		t.Fatal("provider/index validation occurred after runtime connection", rec.Code, rec.Body.String())
	}
	mock.start(t, "")
	before := mock.connections.Load()
	rec = runtimeCall(t, mock.h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"provider": "claude", "state": mock.state, "code": "unused"}))
	if rec.Code != 400 || mock.connections.Load() != before {
		t.Fatal("mismatched provider reached runtime", rec.Code, rec.Body.String())
	}
	rec = runtimeCall(t, mock.h.CancelAuthSession, http.MethodDelete, "/cancel-auth-session?state="+mock.state, "")
	if rec.Code != 200 || !gjson.Get(rec.Body.String(), "cancelled").Bool() {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = runtimeCall(t, mock.h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"state": mock.state, "code": "unused"}))
	if rec.Code != http.StatusNotFound || mock.connections.Load() != before {
		t.Fatal("cancelled session reached runtime", rec.Code, rec.Body.String())
	}
	assertNoRuntimeCallbackFile(t, mock.h.cfg)
}

func TestCodexRuntimeStandardOAuthFailsLocallyAfterModeOff(t *testing.T) {
	for _, operation := range []string{"callback", "status"} {
		t.Run(operation, func(t *testing.T) {
			mock := newRuntimeOAuthMock(t)
			mock.start(t, "")
			rec := runtimeCall(t, mock.h.PutCodexRuntime, http.MethodPatch, "/codex-runtime", `{"enabled":false}`)
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			before := mock.connections.Load()
			if operation == "callback" {
				rec = runtimeCall(t, mock.h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"state": mock.state, "code": "unused"}))
				if rec.Code != 409 {
					t.Fatal(rec.Code, rec.Body.String())
				}
			} else {
				rec = runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
				if rec.Code != 200 || gjson.Get(rec.Body.String(), "status").String() != "error" {
					t.Fatal(rec.Code, rec.Body.String())
				}
			}
			if mock.connections.Load() != before {
				t.Fatal("mode-off session contacted master")
			}
			assertNoRuntimeCallbackFile(t, mock.h.cfg)
		})
	}
}

func TestCodexRuntimeStandardOAuthPreservesRPCFailures(t *testing.T) {
	for _, operation := range []string{"start", "callback", "status"} {
		t.Run(operation, func(t *testing.T) {
			mock := newRuntimeOAuthMock(t)
			if operation != "start" {
				mock.start(t, "")
			}
			mock.fail("cpa/auth/login/" + operation)
			var rec *httptest.ResponseRecorder
			switch operation {
			case "start":
				rec = runtimeCall(t, mock.h.RequestCodexToken, http.MethodGet, "/codex-auth-url", "")
			case "callback":
				rec = runtimeCall(t, mock.h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"state": mock.state, "code": "unused"}))
			case "status":
				rec = runtimeCall(t, mock.h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+mock.state, "")
			}
			wantStatus := http.StatusBadGateway
			if operation == "status" {
				wantStatus = http.StatusOK
			}
			if rec.Code != wantStatus || gjson.Get(rec.Body.String(), "error").String() != "mock OAuth upstream failed" {
				t.Fatal("upstream OAuth failure changed", rec.Code, rec.Body.String())
			}
			assertNoRuntimeCallbackFile(t, mock.h.cfg)
		})
	}
}

func TestCodexRuntimeOffUsesNativeOAuthAndReauthorizesSharedFile(t *testing.T) {
	var masterCalls atomic.Int32
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		masterCalls.Add(1)
		http.Error(w, "master must not be called", 500)
	}))
	defer master.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "mock proxy", 403) }))
	defer proxy.Close()
	h, path := runtimeHandler(t, false, "ws"+strings.TrimPrefix(master.URL, "http"))
	writeRuntimeCredential(t, path, false, map[string]any{"codex_client_system": "windows", "proxy_url": proxy.URL})
	h.syncRuntimeAuth(context.Background(), path)
	auth, found := h.authManager.GetByID("existing-account.json")
	if !found {
		t.Fatal("missing original credential")
	}
	auth.EnsureIndex()
	originalFactory := newCodexOAuthService
	var nativeCalls int
	newCodexOAuthService = func(*config.Config) codexOAuthService {
		nativeCalls++
		return &fakeCodexOAuthService{}
	}
	t.Cleanup(func() { newCodexOAuthService = originalFactory })
	rec := runtimeCall(t, h.RequestCodexToken, http.MethodGet, "/codex-auth-url?auth_index="+auth.Index+"&client_system=mac", "")
	state := gjson.Get(rec.Body.String(), "state").String()
	if rec.Code != 200 || state == "" || gjson.Get(rec.Body.String(), "client_system").String() != "windows" || nativeCalls != 1 {
		t.Fatal("disabled integration changed native OAuth", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { CancelOAuthSession(state) })
	rec = runtimeCall(t, h.PostOAuthCallback, http.MethodPost, "/oauth-callback", runtimeJSON(t, map[string]any{"provider": "codex", "state": state, "code": "native-relogin"}))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	waitForOAuthSessionDone(t, state)
	rec = runtimeCall(t, h.GetAuthStatus, http.MethodGet, "/get-auth-status?state="+state, "")
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	metadata := assertRuntimeEnabled(t, path, false)
	if metadata["access_token"] != "access-native-relogin" || metadata["refresh_token"] != "refresh-native-relogin" || metadata["codex_client_system"] != "windows" || metadata["unknown"] != "retained" {
		t.Fatal("native reauthorization did not update the original shared file", metadata)
	}
	files, err := bridge.ListCredentials(h.cfg)
	if err != nil || len(files) != 1 || files[0].ID != "existing-account.json" {
		t.Fatal("native reauthorization renamed/copied credential", files, err)
	}
	if masterCalls.Load() != 0 {
		t.Fatal("native OAuth contacted master")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("original credential disappeared", err)
	}
}

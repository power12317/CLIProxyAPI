package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexSharedCredentialModeSwitchAndRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.json")
	metadata := map[string]any{"type": "codex", "access_token": "initial", "refresh_token": "initial-refresh", "account_id": "account", "unknown_extension": true}
	codexshared.Set(metadata, codexshared.State{Enabled: true, WorkerID: "worker", Owner: "codex"})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	var rpcCalls, nativeCalls atomic.Int32
	runtimeTokens := make(chan string, 4)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bridge-key" {
			t.Error("missing bridge key")
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
			var msg runtimeTestRPC
			if conn.ReadJSON(&msg) != nil {
				return
			}
			switch msg.Method {
			case "initialize":
				_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{}})
			case "initialized":
			case "cpa/capabilities/read", "cpa/credential/reload":
				_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": bridge.Capabilities{ProtocolVersion: 2, CredentialID: "worker", CredentialFile: "worker.json", AccountID: "account", AuthOwner: "codex", ExecutionMode: "inference-only", RawEvents: true, Operations: []string{"responses"}}})
			case "cpa/inference/start":
				rpcCalls.Add(1)
				latest, _ := codexshared.Read(path)
				runtimeTokens <- latest["access_token"].(string)
				var req bridge.Request
				_ = json.Unmarshal(msg.Params, &req)
				runtimeTestAccept(conn, msg, req)
				runtimeTestFinish(conn, req, runtimeTerminal)
				return
			default:
				t.Error("unexpected method", msg.Method)
				return
			}
		}
	}))
	defer wsServer.Close()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer codex-refreshed" {
			t.Error("CPA did not read latest Codex token")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+runtimeTerminal+"\n\n")
	}))
	defer native.Close()
	cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, Workers: []config.CodexRuntimeWorker{{ID: "worker", URL: "ws" + strings.TrimPrefix(wsServer.URL, "http") + "/cpa/v1/ws", Token: "bridge-key", AuthFile: "worker.json", Models: []string{"runtime-model"}}}}}}
	auth := &coreauth.Auth{ID: "worker.json", FileName: "worker.json", Provider: "codex", Metadata: map[string]any{"access_token": "stale", "refresh_token": "stale"}, Attributes: map[string]string{coreauth.AttributePath: path, coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "base_url": native.URL}}
	executor := NewCodexAutoExecutor(cfg)
	req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":[]}`)}
	if _, err := executor.Execute(context.Background(), auth, req, runtimeTestOptions()); err != nil {
		t.Fatal(err)
	}
	if token := <-runtimeTokens; token != "initial" {
		t.Fatal(token)
	}
	if _, err := executor.Refresh(context.Background(), auth); err == nil {
		t.Fatal("CPA refreshed while Codex owner")
	}
	metadata["access_token"], metadata["refresh_token"] = "codex-refreshed", "codex-rotated"
	_ = codexshared.Write(path, metadata)
	cfg.Codex.Runtime.Enabled = false
	if err := bridge.ReconcileOwners(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), auth, req, runtimeTestOptions()); err != nil {
		t.Fatal(err)
	}
	if nativeCalls.Load() != 1 || rpcCalls.Load() != 1 {
		t.Fatal("disabled runtime was contacted")
	}
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	http.DefaultTransport = forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "auth.openai.com" {
			return original.RoundTrip(r)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "codex-rotated" {
			t.Error("CPA refreshed stale token")
		}
		jwt := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`)) + ".signature"
		body, _ := json.Marshal(map[string]any{"access_token": "cpa-refreshed", "refresh_token": "cpa-rotated", "id_token": jwt, "expires_in": 3600})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if _, err := executor.Refresh(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	latest, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if latest["refresh_token"] != "cpa-rotated" || latest["unknown_extension"] != true {
		t.Fatal(latest)
	}
	cfg.Codex.Runtime.Enabled = true
	if err := bridge.ReconcileOwners(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), auth, req, runtimeTestOptions()); err != nil {
		t.Fatal(err)
	}
	if token := <-runtimeTokens; token != "cpa-refreshed" {
		t.Fatal("Codex did not receive latest CPA token", token)
	}
}

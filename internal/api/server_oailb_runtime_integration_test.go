package api

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

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	tokenstore "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOailbRuntimeSourceRealWorker(t *testing.T) {
	binary := os.Getenv("CODEX_RUNTIME_TEST_BINARY")
	if binary == "" {
		t.Skip("set CODEX_RUNTIME_TEST_BINARY to the v3 codex-app-server with original IPC headers")
	}
	f := newOailbContractFixture(t, true)
	f.cfg.Codex.TurnStateTicket.Models = []string{"gpt-6-astra"}
	store := tokenstore.NewFileTokenStore()
	store.SetBaseDir(f.cfg.AuthDir)
	f.manager.SetStore(store)
	auth := &coreauth.Auth{ID: "runtime-source.json", FileName: "runtime-source.json", Provider: "codex", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, coreauth.AttributePath: filepath.Join(f.cfg.AuthDir, "runtime-source.json")}, Metadata: map[string]any{
		"type": "codex", "codex_cli": map[string]any{"enabled": true}, "email": "source@example.invalid", "account_id": "source-account",
		"access_token": "synthetic-worker-token", "refresh_token": "synthetic-refresh", "last_refresh": time.Now().UTC().Format(time.RFC3339),
		"id_token": base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fake-user","email":"source@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"source-account","chatgpt_user_id":"fake-user","chatgpt_plan_type":"pro"}}`)) + ".signature",
	}}
	if _, err := f.manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	harvester := helps.NewCodexTurnStateTicketHarvester(helps.CodexTurnStateTicketHarvesterOptions{Config: func() *config.Config { return f.cfg }, List: f.manager.List, Update: func(ctx context.Context, base, updated *coreauth.Auth) error {
		t.Error("runtime source ticket must not update auth JSON")
		return nil
	}})
	t.Cleanup(func() { helps.InvalidateCodexTurnStateTickets(auth.ID) })
	helps.SetCodexTurnStateTicketRecorder(harvester.Record)
	t.Cleanup(func() { helps.SetCodexTurnStateTicketRecorder(nil) })
	var workerCalls, directCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		n := workerCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-worker-token" {
			t.Error("worker lost its own OAuth identity")
		}
		if gjson.GetBytes(raw, "captureTicketSnapshot").Exists() {
			t.Error("unexpected capture flag in business payload")
		}
		state := oailbContractState(time.Now().Truncate(time.Second), byte(n))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Set-Cookie", "__cflb=source-cf; Path=/; Max-Age=3600")
		w.Header().Add("Set-Cookie", "__oailb=source-oai; Path=/; Max-Age=3600")
		model := gjson.GetBytes(raw, "model").String()
		if model == "gpt-6-astra" {
			w.Header().Set("X-Codex-Turn-State", state)
		} else {
			event, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": state}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"runtime-source-result\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}]}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	endpoint := startOailbSourceWorker(t, binary, f.cfg.AuthDir, auth.ID, upstream.URL)
	// A transparent fixture inspects the actual wire without changing its protocol.
	var rpcCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = client.Close() }()
		server, _, err := (&websocket.Dialer{}).DialContext(r.Context(), endpoint, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = server.Close() }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = client.Close() }()
			for {
				kind, raw, errRead := server.ReadMessage()
				if errRead != nil {
					return
				}
				if gjson.GetBytes(raw, "method").String() == "cpa/inference/ticket" {
					t.Error("test unexpectedly depended on the separate ticket notification")
				}
				if client.WriteMessage(kind, raw) != nil {
					return
				}
			}
		}()
		for {
			kind, raw, errRead := client.ReadMessage()
			if errRead != nil {
				break
			}
			if gjson.GetBytes(raw, "method").String() == "cpa/inference/start" {
				rpcCalls.Add(1)
				if gjson.GetBytes(raw, "params.captureTicketSnapshot").Exists() {
					t.Error("CPA added the forbidden capture parameter")
				}
			}
			if server.WriteMessage(kind, raw) != nil {
				break
			}
		}
		_ = server.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	f.cfg.Codex.Runtime = config.CodexRuntimeConfig{Enabled: true, URL: "ws" + strings.TrimPrefix(proxy.URL, "http")}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", oailbContractTransport(func(*http.Request) (*http.Response, error) {
		directCalls.Add(1)
		return nil, fmt.Errorf("CPA source must never contact upstream directly")
	}))
	a := runtimeexecutor.NewCodexAutoExecutor(f.cfg)
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-luna"} {
		for _, stream := range []bool{false, true} {
			current, _ := f.manager.GetByID(auth.ID)
			req := coreexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","input":[{"role":"user","content":"ping"}]}`)}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			if stream {
				result, err := a.ExecuteStream(ctx, current, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := a.Execute(ctx, current, req, opts); err != nil {
				t.Fatal(err)
			}
			current, _ = f.manager.GetByID(auth.ID)
			ticket, err := helps.ExportCodexSourceTicket(current, model, time.Now())
			if err != nil || ticket.Route.CFLB.Value != "source-cf" || ticket.Route.OaiLB.Value != "source-oai" {
				t.Fatalf("runtime response was not cached for %s: %v", model, err)
			}
		}
	}
	if workerCalls.Load() != 4 || rpcCalls.Load() != 4 || directCalls.Load() != 0 {
		t.Fatalf("normal-response capture sent extra requests: worker=%d rpc=%d direct=%d", workerCalls.Load(), rpcCalls.Load(), directCalls.Load())
	}
	listing := f.request(t, http.MethodGet, "/v0/management/auth-files", "", true, 200)
	if !gjson.GetBytes(listing, `files.0.codex_turn_tickets.#(model=="gpt-6-astra").ready`).Bool() {
		t.Fatal("the existing credential page did not expose runtime source readiness")
	}
	// Reload unchanged credentials; the source remains available from memory.
	loaded, err := store.List(t.Context())
	if err != nil || len(loaded) != 1 {
		t.Fatal("credential was not retained on disk", err)
	}
	for key := range loaded[0].Metadata {
		if helps.IsCodexTurnStateTicketMetadataKey(key) {
			t.Fatal("runtime response persisted ticket material")
		}
	}
	rawAuth, errRead := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if errRead != nil || bytes.Contains(rawAuth, []byte("codex_turn_ticket:")) {
		t.Fatalf("runtime ticket leaked to disk: %v", errRead)
	}
	reloaded, err := f.manager.Update(coreauth.WithSkipPersist(t.Context()), loaded[0])
	if err != nil {
		t.Fatal(err)
	}
	seed, err := helps.ExportCodexSourceTicket(reloaded, "gpt-6-astra", time.Now())
	if err != nil {
		t.Fatal("reloaded runtime source is not exportable", err)
	}
	var sourceReads atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceReads.Add(1)
		f.server.engine.ServeHTTP(w, r)
	}))
	t.Cleanup(source.Close)
	b := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}, CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: source.URL, SourceAuthID: auth.ID, SourceManagementKey: oailbContractKey}}}
	borrower := &coreauth.Auth{ID: t.Name() + "-borrower", Provider: "codex", Metadata: map[string]any{"access_token": "borrower-token", "account_id": "borrower-account"}}
	t.Cleanup(func() { helps.InvalidateCodexCookieJar(borrower.ID) })
	targetState := oailbContractState(time.Now().Truncate(time.Second), 100)
	var shots atomic.Int32
	bctx := context.WithValue(t.Context(), "cliproxy.roundtripper", oailbContractTransport(func(r *http.Request) (*http.Response, error) {
		n := shots.Add(1)
		if r.Header.Get("Authorization") != "Bearer borrower-token" {
			t.Error("B used A's OAuth identity")
		}
		for name, want := range map[string]string{"__cflb": "source-cf", "__oailb": "source-oai"} {
			cookie, err := r.Cookie(name)
			if err != nil || cookie.Value != want {
				t.Error("B did not use the runtime source cookie pair", name)
			}
		}
		want := targetState
		if n == 1 {
			want = seed.State
		}
		if r.Header.Get("X-Codex-Turn-State") != want {
			t.Error("B used the wrong ticket generation")
		}
		h := http.Header{"Content-Type": {"text/event-stream"}}
		if n == 1 {
			h.Set("X-Codex-Turn-State", targetState)
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"borrow-success\",\"status\":\"completed\",\"output\":[]}}\n\n")), Request: r}, nil
	}))
	for range 2 {
		if _, err := runtimeexecutor.NewCodexExecutor(b).Execute(bctx, borrower, coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); err != nil {
			t.Fatal("B could not borrow from runtime source A", err)
		}
	}
	if shots.Load() != 4 || sourceReads.Load() != 1 || workerCalls.Load() != 4 {
		t.Fatalf("B did not reuse, or borrowing caused A to probe: B=%d reads=%d A=%d", shots.Load(), sourceReads.Load(), workerCalls.Load())
	}
	// Exercise the same runtime acquisition used by the scheduler; timer boundaries
	// are covered with a controlled clock in the harvester tests.
	current, _ := f.manager.GetByID(auth.ID)
	seed.ExpiresAt = time.Now().Add(9 * time.Minute)
	helps.StoreCodexTurnStateTicket(current, seed)
	replacement, status, err := helps.HarvestCodexTurnStateTicket(ctx, f.cfg, current, seed.Model, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("runtime renewal failed: status=%d err=%v", status, err)
	}
	harvester.Record(current, replacement)
	current, _ = f.manager.GetByID(auth.ID)
	renewed, err := helps.ExportCodexSourceTicket(current, "gpt-6-astra", time.Now())
	if err != nil || renewed.State == seed.State || workerCalls.Load() != 5 || rpcCalls.Load() != 5 || directCalls.Load() != 0 {
		t.Fatalf("runtime renewal failed: %v worker=%d rpc=%d direct=%d", err, workerCalls.Load(), rpcCalls.Load(), directCalls.Load())
	}
	helps.InvalidateCodexTurnStateTickets(auth.ID)
	if _, err := helps.ExportCodexSourceTicket(reloaded, "gpt-6-astra", time.Now()); err == nil {
		t.Fatal("cleared memory was restored from the credential file")
	}
	t.Log("A: four ordinary v3 requests plus one scheduled renewal; zero CPA direct source calls. B: one source read, two probes, two business requests. No new RPC field or ticket notification.")
}

func startOailbSourceWorker(t *testing.T, binary, authDir, authID, upstream string) string {
	t.Helper()
	home := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	state := filepath.Join(home, "state")
	accountHome := filepath.Join(state, authID)
	if err := os.MkdirAll(accountHome, 0700); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("model = \"gpt-6-astra\"\nmodel_provider = \"fixture\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nchatgpt_base_url = %q\n[model_providers.fixture]\nname = \"OpenAI\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", upstream, upstream+"/v1")
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), []byte(toml), 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := os.Create(filepath.Join(home, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + state, "RUST_LOG=error", "CODEX_CPA_AUTH_DIR=" + authDir, "CODEX_CPA_PORT=" + strconv.Itoa(port), "CODEX_APP_SERVER_MANAGED_CONFIG_PATH=" + filepath.Join(home, "managed.toml"), "CODEX_APP_SERVER_LOGIN_ISSUER=" + upstream, "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + upstream + "/oauth/token"}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		_ = logs.Close()
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
		_ = logs.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(logs.Name())
			t.Logf("worker diagnostics: %s", raw)
		}
	})
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/cpa/v1/ws", port)
	startup, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	ticks := time.NewTicker(20 * time.Millisecond)
	defer ticks.Stop()
	for {
		select {
		case <-startup.Done():
			t.Fatal("worker did not start")
		case <-ticks.C:
			client, err := bridge.Dial(startup, endpoint)
			if err == nil {
				client.Close()
				return endpoint
			}
		}
	}
}

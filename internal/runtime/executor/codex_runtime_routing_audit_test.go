package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Regression tests adapted from the external routing audit: all runtime failures must remain local-network only.
func auditSharedAuth(t *testing.T, cfg *config.Config, auth *coreauth.Auth, baseURL string) {
	t.Helper()
	cfg.AuthDir = t.TempDir()
	path := filepath.Join(cfg.AuthDir, auth.ID)
	auth.Metadata["access_token"] = "audit-synthetic-token"
	auth.Attributes[coreauth.AttributePath] = path
	auth.Attributes["base_url"] = baseURL
	if err := codexshared.Write(path, auth.Metadata); err != nil {
		t.Fatal(err)
	}
	if !coreauth.IsCodexRuntimeOwnedAuth(auth) {
		t.Fatal("fixture must be owned by the runtime")
	}
}

func TestRuntimeRoutingAuditImagesRejectUnavailableMaster(t *testing.T) {
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				var nativeCalls, masterCalls atomic.Int32
				native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					nativeCalls.Add(1)
					if r.URL.Path != strings.TrimPrefix(path, "/v1") {
						t.Errorf("upstream path = %s", r.URL.Path)
					}
					http.Error(w, "audit-upstream-reached", http.StatusBadRequest)
				}))
				t.Cleanup(native.Close)
				master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					masterCalls.Add(1)
					http.Error(w, "audit-master-unavailable", http.StatusServiceUnavailable)
				}))
				t.Cleanup(master.Close)
				cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, URL: "ws" + strings.TrimPrefix(master.URL, "http")}}}
				auth := runtimeTestAuth("audit-image.json")
				auditSharedAuth(t, cfg, auth, native.URL)
				req := coreexecutor.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"audit","images":[{"image_url":"data:image/png;base64,AA=="}]}`)}
				opts := codexOpenAIImageTestOptions(path, stream)
				auto := NewCodexAutoExecutor(cfg)
				var err error
				if stream {
					_, err = auto.ExecuteStream(context.Background(), auth, req, opts)
				} else {
					_, err = auto.Execute(context.Background(), auth, req, opts)
				}
				if err == nil || !strings.Contains(err.Error(), "Codex runtime connection failed") {
					t.Fatalf("expected the runtime connection error, got %v", err)
				}
				if nativeCalls.Load() != 0 || masterCalls.Load() != 1 {
					t.Fatalf("native=%d master=%d", nativeCalls.Load(), masterCalls.Load())
				}
				t.Logf("runtime enabled and credential owned: CPA upstream HTTP=%d, master connections=%d", nativeCalls.Load(), masterCalls.Load())
			})
		}
	}
}

func TestRuntimeRoutingAuditCompactRejectedWithoutDirectFallback(t *testing.T) {
	var inferenceCalls, capabilityCalls, nativeCalls atomic.Int32
	runtimeCfg, auth := runtimeTestMaster(t, "audit-compact.json", func(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
		inferenceCalls.Add(1)
	}, func(caps *bridge.Capabilities) { capabilityCalls.Add(1) })
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		http.Error(w, "unexpected-direct-upstream", http.StatusInternalServerError)
	}))
	t.Cleanup(native.Close)
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: runtimeCfg}}
	auditSharedAuth(t, cfg, auth, native.URL)
	opts := runtimeTestOptions()
	opts.Alt = "responses/compact"
	_, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":[]}`)}, opts)
	if err == nil || !strings.Contains(err.Error(), "Codex runtime operation is unsupported") {
		t.Fatalf("unexpected compact result: %v", err)
	}
	if capabilityCalls.Load() != 1 || inferenceCalls.Load() != 0 || nativeCalls.Load() != 0 {
		t.Fatalf("capabilities=%d inference=%d native=%d", capabilityCalls.Load(), inferenceCalls.Load(), nativeCalls.Load())
	}
	t.Logf("capabilities=%d, inference/start=%d, CPA upstream HTTP=%d; error=%v", capabilityCalls.Load(), inferenceCalls.Load(), nativeCalls.Load(), err)
}

func TestRuntimeRoutingAuditUnavailableDoesNotFallbackForResponses(t *testing.T) {
	var nativeCalls, masterCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		http.Error(w, "unexpected-direct-upstream", http.StatusInternalServerError)
	}))
	t.Cleanup(native.Close)
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		masterCalls.Add(1)
		http.Error(w, "audit-master-unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(master.Close)
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, URL: "ws" + strings.TrimPrefix(master.URL, "http")}}}
	auth := runtimeTestAuth("audit-responses.json")
	auditSharedAuth(t, cfg, auth, native.URL)
	_, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":[]}`)}, runtimeTestOptions())
	if err == nil || masterCalls.Load() != 1 || nativeCalls.Load() != 0 {
		t.Fatalf("master=%d native=%d error=%v", masterCalls.Load(), nativeCalls.Load(), err)
	}
	t.Logf("master connections=%d, CPA upstream HTTP=%d; error=%v", masterCalls.Load(), nativeCalls.Load(), err)
}

func TestRuntimeRoutingAuditCompactionTriggerUsesResponses(t *testing.T) {
	var inferenceCalls, nativeCalls atomic.Int32
	terminal := `{"type":"response.completed","response":{"id":"audit-compaction","object":"response","status":"completed","output":[{"type":"compaction","encrypted_content":"audit-synthetic-compaction"}],"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25}}}`
	runtimeCfg, auth := runtimeTestMaster(t, "audit-trigger.json", func(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
		inferenceCalls.Add(1)
		if req.Operation != "responses" {
			t.Errorf("operation = %s", req.Operation)
		}
		found := false
		for _, item := range gjson.GetBytes(req.Request, "input").Array() {
			found = found || item.Get("type").String() == "compaction_trigger"
		}
		if !found {
			t.Error("CPA dropped the compaction trigger")
		}
		runtimeTestAccept(c, m, req)
		runtimeTestFinish(c, req, terminal)
	})
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		http.Error(w, "unexpected-direct-upstream", http.StatusInternalServerError)
	}))
	t.Cleanup(native.Close)
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: runtimeCfg}}
	auditSharedAuth(t, cfg, auth, native.URL)
	req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":[{"role":"user","content":"audit history"},{"type":"compaction_trigger"}]}`)}
	response, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, req, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if inferenceCalls.Load() != 1 || nativeCalls.Load() != 0 || gjson.GetBytes(response.Payload, "output.0.type").String() != "compaction" {
		t.Fatalf("inference=%d native=%d output type=%s", inferenceCalls.Load(), nativeCalls.Load(), gjson.GetBytes(response.Payload, "output.0.type").String())
	}
	t.Logf("operation=responses, compaction_trigger preserved, inference/start=%d, CPA upstream HTTP=%d, output=compaction", inferenceCalls.Load(), nativeCalls.Load())
}

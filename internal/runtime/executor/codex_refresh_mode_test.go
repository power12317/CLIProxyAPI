package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexGlobalRefreshModeUsesLatestFileTokens(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("refresh_token") != "worker-latest-refresh" {
			t.Error("CPA refreshed using stale in-memory tokens")
		}
		body, _ := json.Marshal(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "id_token": makeTestCodexRefreshJWT("pro", "account"), "expires_in": 3600})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	for _, flag := range []string{"missing", "false", "disabled"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			metadata := map[string]any{"type": "codex", "access_token": "worker-access", "refresh_token": "worker-latest-refresh", "extension": "retained"}
			if flag != "missing" {
				metadata["codex_cli"] = map[string]any{"enabled": flag == "disabled"}
			}
			metadata["disabled"] = flag == "disabled"
			path := filepath.Join(dir, "original.json")
			if err := codexshared.Write(path, metadata); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
			executor := NewCodexExecutor(cfg)
			auth := &coreauth.Auth{ID: "original.json", Provider: "codex", Metadata: map[string]any{"access_token": "stale", "refresh_token": "stale"}}
			before := calls
			if _, err := executor.Refresh(context.Background(), auth); err == nil || calls != before {
				t.Fatal("global mode sent an OAuth refresh request")
			}
			cfg.Codex.Runtime.Enabled = false
			if err := bridge.ReconcileState(cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Refresh(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			latest, err := codexshared.Read(path)
			if err != nil || latest["access_token"] != "new-access" || latest["extension"] != "retained" || calls != before+1 {
				t.Fatal("handoff did not preserve the original file and latest tokens", err)
			}
		})
	}
}

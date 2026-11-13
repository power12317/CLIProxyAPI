package auth

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexRuntimeSelectionUsesOrdinaryPartitionOnlyForActiveWorker(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, worker := range []bool{false, true} {
			for _, office := range []bool{false, true} {
				t.Run(fmt.Sprintf("global=%t/worker=%t/office=%t", global, worker, office), func(t *testing.T) {
					cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: global}, Basispoints: config.CodexBasispointsConfig{Enabled: office}}}
					m := NewManager(nil, nil, nil)
					m.SetConfig(cfg)
					m.RegisterExecutor(&countingRefreshExecutor{id: "codex"})
					auth := &Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "expired", "expired": "2100-01-01T00:00:00Z", "use_siwc": true, "siwc": map[string]any{"access_token": "unused-siwc", "expired": "2000-01-01T00:00:00Z"}, "use_basispoints": true, "codex_cli": map[string]any{"enabled": worker}}}
					if _, err := m.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					selected, err := m.SelectAuth(t.Context(), "codex", "runtime-model", core.Options{})
					want := global && worker && !office
					if (err == nil && selected != nil) != want {
						t.Fatalf("selected=%t err=%v, want %t", selected != nil, err, want)
					}
					// A hot mode change must immediately restore SIWC expiration checks, even
					// before the file watcher has cleared a stale worker flag.
					m.SetConfig(&config.Config{})
					if _, err := m.SelectAuth(t.Context(), "codex", "runtime-model", core.Options{}); err == nil {
						t.Fatal("disabled runtime bypassed local token expiration")
					}
				})
			}
		}
	}
}

func TestCodexRuntimeSelectionRetainsCredentialAndQuotaBlocks(t *testing.T) {
	for _, reason := range []string{"disabled", "quota", "unauthorized"} {
		t.Run(reason, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}})
			m.RegisterExecutor(&countingRefreshExecutor{id: "codex"})
			auth := &Auth{ID: t.Name(), Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "expired", "expired": "2100-01-01T00:00:00Z", "codex_cli": map[string]any{"enabled": true}}}
			switch reason {
			case "disabled":
				auth.Disabled = true
			case "quota":
				auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
			case "unauthorized":
				auth.Status, auth.Unavailable, auth.LastError = StatusError, true, &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized"}
			}
			if _, err := m.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, err := m.SelectAuth(t.Context(), "codex", "runtime-model", core.Options{}); err == nil {
				t.Fatal("worker mode bypassed " + reason)
			}
		})
	}
}

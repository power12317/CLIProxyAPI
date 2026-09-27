package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestCodexRuntimeRegistrationAndRemoval(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, Workers: []config.CodexRuntimeWorker{{ID: "service-test", Socket: "/run/codex.sock", AccountID: "account", Models: []string{"model"}, Prefix: "runtime"}}}}}
	m := coreauth.NewManager(nil, nil, nil)
	s := &Service{cfg: cfg, coreManager: m}
	reg := internalregistry.GetGlobalRegistry()
	id := "codex-runtime:service-test"
	t.Cleanup(func() { reg.UnregisterClient(id) })
	s.registerConfigAPIKeyAuths(ctx, cfg)
	if _, ok := m.Executor(coreauth.CodexRuntimeProvider); !ok {
		t.Fatal("runtime executor missing at cold start")
	}
	if a, ok := m.GetByID(id); !ok || a.Metadata != nil {
		t.Fatal("runtime reference not registered")
	}
	models := codexModelIDSet(reg.GetModelsForClient(id))
	if _, ok := models["runtime/model"]; !ok {
		t.Fatalf("prefixed model missing: %v", models)
	}
	cfg.Codex.Runtime.Workers[0].Disabled = true
	s.registerConfigAPIKeyAuths(ctx, cfg)
	if len(reg.GetModelsForClient(id)) != 0 {
		t.Fatal("disabled worker still advertised models")
	}
	cfg.Codex.Runtime.Workers[0].Disabled = false
	s.registerConfigAPIKeyAuths(ctx, cfg)
	if len(reg.GetModelsForClient(id)) == 0 {
		t.Fatal("re-enabled worker missing models")
	}
	cfg.Codex.Runtime.Workers = nil
	s.registerConfigAPIKeyAuths(ctx, cfg)
	if _, ok := m.GetByID(id); ok {
		t.Fatal("removed worker remains selectable")
	}
	if len(reg.GetModelsForClient(id)) != 0 {
		t.Fatal("removed worker models remain")
	}
}

func TestCodexRuntimeInvalidHotReloadDoesNotCommit(t *testing.T) {
	old := &config.Config{}
	s := &Service{cfg: old}
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, Workers: []config.CodexRuntimeWorker{{ID: "bad"}}}}}
	if commit := s.commitConfigUpdate(cfg); commit.cfg != nil {
		t.Fatal("invalid config committed")
	}
	if s.cfg != old {
		t.Fatal("active config replaced")
	}
}

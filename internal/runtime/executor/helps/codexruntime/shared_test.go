package codexruntime

import (
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexSharedStandaloneConfigRestoresCPAOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	metadata := map[string]any{"type": "codex", "access_token": "latest", "refresh_token": "latest-refresh"}
	codexshared.Set(metadata, codexshared.State{Enabled: true, WorkerID: "removed-worker", Owner: "codex"})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileOwners(&config.Config{AuthDir: dir}); err != nil {
		t.Fatal(err)
	}
	got, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := codexshared.Get(got)
	if state.Owner != "cpa" || !state.Enabled || got["access_token"] != "latest" || got["refresh_token"] != "latest-refresh" {
		t.Fatal("standalone CPA did not retain shared credentials", got)
	}
}

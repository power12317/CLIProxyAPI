package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSharedCodexStatusSavePreservesFileTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	metadata := map[string]any{"type": "codex", "access_token": "new-token", "refresh_token": "new-refresh", "extra": "keep"}
	codexshared.Set(metadata, codexshared.State{Enabled: true, WorkerID: "worker", Owner: "cpa"})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &coreauth.Auth{ID: "codex.json", Provider: "codex", FileName: "codex.json", Metadata: map[string]any{"type": "codex", "access_token": "stale", "refresh_token": "stale", "label": "edited"}}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	got, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "new-token" || got["refresh_token"] != "new-refresh" || got["extra"] != "keep" || got["label"] != "edited" {
		t.Fatal(got)
	}
}

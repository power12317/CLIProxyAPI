package auth

import (
	"context"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSharedCodexStatusSavePreservesFileTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	metadata := map[string]any{"type": "codex", "access_token": "new-token", "refresh_token": "new-refresh", "extra": "keep"}
	codexshared.Set(metadata, codexshared.State{Enabled: true})
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

func TestSharedCodexReauthorizationReplacesTokensInOriginalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Original Account.json")
	metadata := map[string]any{"type": "codex", "access_token": "old", "refresh_token": "old-refresh", "extension": true, "codex_client_system": "windows"}
	codexshared.Set(metadata, codexshared.State{Enabled: false})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &coreauth.Auth{ID: "Original Account.json", FileName: "Original Account.json", Provider: "codex", Metadata: map[string]any{"email": "new@example.invalid"}, Storage: &codexauth.CodexTokenStorage{AccessToken: "new-access", RefreshToken: "new-refresh", Email: "new@example.invalid"}}
	saved, err := store.Save(coreauth.WithAuthCreationIntent(context.Background()), auth)
	if err != nil || saved != path {
		t.Fatal(saved, err)
	}
	got, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	state, exists := codexshared.Get(got)
	if got["access_token"] != "new-access" || got["refresh_token"] != "new-refresh" || got["extension"] != true || got["codex_client_system"] != "windows" || !exists || state.Enabled {
		t.Fatal(got)
	}
}

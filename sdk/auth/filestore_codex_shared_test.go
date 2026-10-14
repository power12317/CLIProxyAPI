package auth

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSharedCodexStatusSurvivesManagerRestart(t *testing.T) {
	for _, runtimeEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("runtime=%t", runtimeEnabled), func(t *testing.T) {
			dir := t.TempDir()
			const id = "original-account.json"
			path := filepath.Join(dir, id)
			metadata := map[string]any{
				"type": "codex", "disabled": true, "access_token": "old-access",
				"refresh_token": "old-refresh", "id_token": "old-id", "extension": "keep",
				"codex_cli": map[string]any{"enabled": runtimeEnabled},
			}
			if err := codexshared.Write(path, metadata); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			loadManager := func() *coreauth.Manager {
				t.Helper()
				store := NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				manager.SetConfig(&config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: runtimeEnabled}}})
				if err := manager.Load(ctx); err != nil {
					t.Fatal(err)
				}
				return manager
			}
			manager := loadManager()
			for index, disabled := range []bool{false, true, false} {
				auth, ok := manager.GetByID(id)
				if !ok {
					t.Fatal("credential missing after restart")
				}
				// The worker's newer tokens must survive a CPA status update from an older snapshot.
				for _, key := range []string{"access_token", "refresh_token", "id_token"} {
					metadata[key] = fmt.Sprintf("new-%s-%d", key, index)
				}
				if err := codexshared.Write(path, metadata); err != nil {
					t.Fatal(err)
				}
				auth.Disabled = disabled
				auth.Metadata["disabled"] = disabled
				auth.Status = coreauth.StatusActive
				if disabled {
					auth.Status = coreauth.StatusDisabled
				}
				if _, err := manager.Update(ctx, auth); err != nil {
					t.Fatal(err)
				}
				metadata["disabled"] = disabled
				saved, err := codexshared.Read(path)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(saved, metadata) {
					t.Fatalf("status update did not preserve the shared file: got=%v want=%v", saved, metadata)
				}
				manager = loadManager()
				reloaded, ok := manager.GetByID(id)
				if !ok || reloaded.Disabled != disabled || reloaded.Status != auth.Status || !reflect.DeepEqual(reloaded.Metadata, metadata) {
					t.Fatalf("status changed after restart: got=%+v disabled=%t", reloaded, disabled)
				}
			}
		})
	}
}

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

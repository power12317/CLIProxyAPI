package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexRuntimeSharedCredentialRefreshFollowsGlobalMode(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{}
	m := NewManager(store, nil, nil)
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
	m.SetConfig(cfg)
	executor := &countingRefreshExecutor{id: "codex"}
	m.RegisterExecutor(executor)
	id := "Original CPA Credential.JSON"
	path := filepath.Join(t.TempDir(), id)
	metadata := map[string]any{"type": "codex", "access_token": "synthetic", "refresh_token": "synthetic", "expired": "2000-01-01T00:00:00Z", "refresh_interval_seconds": 60, "unknown_extension": "preserved"}
	codexshared.Set(metadata, codexshared.State{Enabled: true})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	readAuth := func() *Auth {
		t.Helper()
		data, err := codexshared.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return &Auth{ID: id, FileName: id, Provider: "codex", Attributes: map[string]string{AttributePath: path, AttributeAuthKind: AuthKindOAuth}, Metadata: data}
	}
	a := readAuth()
	if _, err := m.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, scheduled := m.nextRefreshCheckAt(time.Now(), a, time.Minute); scheduled || m.shouldRefresh(a, time.Now()) {
		t.Fatal("enabled shared credential entered CPA refresh scheduling")
	}
	if _, err := m.ForceRefreshAuth(ctx, a.ID); err == nil {
		t.Fatal("forced refresh was accepted while Codex maintained the credential")
	}
	if got := m.ForceRefreshAll(ctx); len(got) != 0 {
		t.Fatal("batch refresh included enabled shared credential")
	}
	if _, err := m.refreshAuthForRequest(ctx, a.ID, "synthetic"); err == nil {
		t.Fatal("request refresh was accepted while Codex maintained the credential")
	}
	m.refreshAuth(ctx, a.ID)
	m.MarkResult(ctx, Result{AuthID: a.ID, Provider: "codex", Model: "model", Success: true})
	if executor.refreshCalls.Load() != 0 || store.saveCount.Load() != 0 {
		t.Fatal("CPA refreshed or persisted enabled shared credential tokens")
	}

	// The same original file becomes CPA-managed after the global mode changes.
	cfg.Codex.Runtime.Enabled = false
	m.SetConfig(cfg)
	codexshared.Set(metadata, codexshared.State{Enabled: false})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	a = readAuth()
	if _, err := m.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, scheduled := m.nextRefreshCheckAt(time.Now(), a, time.Minute); !scheduled || !m.shouldRefresh(a, time.Now()) || !authHasRefreshCredential(a) {
		t.Fatal("disabled runtime credential did not return to CPA refresh scheduling")
	}
	m.refreshAuth(ctx, a.ID)
	if _, err := m.ForceRefreshAuth(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.ForceRefreshAll(ctx); len(got) != 1 || got[0].ID != id || !got[0].Success {
		t.Fatalf("batch refresh did not resume for original ID: %#v", got)
	}
	if _, err := m.refreshAuthForRequest(ctx, a.ID, "refreshed-token"); err != nil {
		t.Fatal(err)
	}
	if executor.refreshCalls.Load() != 4 || store.saveCount.Load() == 0 {
		t.Fatalf("CPA refresh/persistence did not resume: refresh=%d save=%d", executor.refreshCalls.Load(), store.saveCount.Load())
	}
	if a.ID != id || a.FileName != id || a.Metadata["unknown_extension"] != "preserved" {
		t.Fatal("mode change altered original credential identity or metadata")
	}
}

func TestCodexRuntimeOwnershipUsesSharedFlagOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		enabled  bool
		disabled bool
		want     bool
	}{
		{"enabled shared credential", "codex", true, false, true},
		{"disabled runtime", "codex", false, false, false},
		{"disabled credential", "codex", true, true, false},
		{"unrelated provider", "claude", true, false, false},
		{"legacy virtual identity", CodexRuntimeProvider, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]any{"disabled": tc.disabled}
			codexshared.Set(metadata, codexshared.State{Enabled: tc.enabled})
			if got := IsCodexRuntimeOwnedAuth(&Auth{ID: "same.json", Provider: tc.provider, Metadata: metadata}); got != tc.want {
				t.Fatalf("ownership = %v, want %v", got, tc.want)
			}
		})
	}
}

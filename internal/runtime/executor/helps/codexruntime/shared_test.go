package codexruntime

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexModeOffWaitsForMasterAndRetriesFailedApply(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir(), Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
	path := CredentialPath(cfg, "original.json")
	if err := codexshared.Write(path, map[string]any{"type": "codex", "access_token": "worker-access"}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	stale := cfg.CloneForRuntime()
	var calls atomic.Int32
	cfg.Codex.Runtime.URL = serveMaster(t, validCaps(), func(conn *websocket.Conn, msg message) {
		if msg.Method != "cpa/credential/reload" || !Enabled(cfg) {
			t.Error("CPA regained refresh ownership before master reload completed")
		}
		if calls.Add(1) == 1 {
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "stop failed", "data": map[string]any{"httpStatus": 500}}})
			return
		}
		_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{}})
	})
	cfg.Codex.Runtime.Enabled = false
	if err := ApplyConfig(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "reload Codex master") {
		t.Fatal("failed reload did not identify the failing step", err)
	}
	if !Enabled(cfg) {
		t.Fatal("failed handoff resumed CPA refresh")
	}
	if err := ApplyConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || Enabled(cfg) || Enabled(stale) {
		t.Fatal("successful retry did not update the live refresh owner")
	}
	if err := ApplyConfig(context.Background(), cfg); err != nil || calls.Load() != 2 {
		t.Fatal("unchanged applied mode reloaded master", err)
	}
}

func TestCodexSharedModeAndOriginalFileIdentity(t *testing.T) {
	dir := t.TempDir()
	id := "team/中文 Original.JSON"
	path := filepath.Join(dir, id)
	metadata := map[string]any{"type": "codex", "access_token": "latest", "refresh_token": "latest-refresh", "extension": true, "codex_cli": map[string]any{"enabled": true, "owner": "codex", "worker_id": "old"}}
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{CredentialModes: map[string]bool{id: true}}}}
	check := func(enabled bool) {
		t.Helper()
		got, err := codexshared.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := codexshared.Get(got)
		flags := got["codex_cli"].(map[string]any)
		if state.Enabled != enabled || got["access_token"] != "latest" || got["extension"] != true || flags["owner"] != nil || flags["worker_id"] != nil {
			t.Fatal(got)
		}
	}
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	check(false)
	cfg.Codex.Runtime.Enabled = true
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	check(true)
	creds, err := ListCredentials(cfg)
	if err != nil || len(creds) != 1 || creds[0].ID != id {
		t.Fatal(creds, err)
	}
	fresh, pathRead, err := LoadSharedAuth(cfg, &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "stale"}})
	if err != nil || pathRead != path || fresh.Metadata["access_token"] != "latest" {
		t.Fatal(fresh, pathRead, err)
	}
	// A direct edit remains effective across unrelated reloads.
	metadata, _ = codexshared.Read(path)
	codexshared.Set(metadata, codexshared.State{Enabled: false})
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	check(false)
	cfg.Codex.Runtime.Enabled = false
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	check(false)
	fresh, _, err = LoadSharedAuth(cfg, &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "stale"}})
	if err != nil || fresh.Metadata["access_token"] != "latest" {
		t.Fatal("disabled mode lost latest token", err)
	}
}

func TestCodexSharedOffNeedsNoMaster(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir(), Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{URL: "ws://127.0.0.1:1"}}}
	if err := ApplyConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSharedPreferenceChangeLeavesOtherFileToggleAlone(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir(), Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, CredentialModes: map[string]bool{"a.json": true, "b.json": false}}}}
	for _, id := range []string{"a.json", "b.json"} {
		if err := codexshared.Write(CredentialPath(cfg, id), map[string]any{"type": "codex"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := codexshared.Read(CredentialPath(cfg, "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	codexshared.Set(first, codexshared.State{Enabled: false})
	if err := codexshared.Write(CredentialPath(cfg, "a.json"), first); err != nil {
		t.Fatal(err)
	}
	cfg.Codex.Runtime.CredentialModes["b.json"] = true
	if err := ReconcileState(cfg); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"a.json": false, "b.json": true} {
		metadata, err := codexshared.Read(CredentialPath(cfg, id))
		if err != nil {
			t.Fatal(err)
		}
		state, _ := codexshared.Get(metadata)
		if state.Enabled != want {
			t.Fatalf("%s enabled=%v want=%v", id, state.Enabled, want)
		}
	}
}

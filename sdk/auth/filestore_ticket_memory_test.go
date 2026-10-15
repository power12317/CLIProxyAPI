package auth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileTokenStoreDoesNotRewriteLegacyTickets(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "runtime-shared"}[shared], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.json")
			metadata := map[string]any{"type": "codex", "access_token": "original-token", "note": "keep", "codex_turn_ticket:model": map[string]any{"state": "legacy-ticket", "route": "legacy-cookie"}}
			if shared {
				metadata["codex_cli"] = map[string]any{"enabled": true}
			}
			raw, _ := json.Marshal(metadata)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := NewFileTokenStore()
			store.SetBaseDir(dir)
			loaded, err := store.List(t.Context())
			if err != nil || len(loaded) != 1 {
				t.Fatalf("load: %v, count=%d", err, len(loaded))
			}
			if _, exists := loaded[0].Metadata["codex_turn_ticket:model"]; exists {
				t.Fatal("loaded legacy ticket")
			}
			unchanged, _ := os.ReadFile(path)
			if !bytes.Equal(raw, unchanged) {
				t.Fatal("loading credentials rewrote the source file")
			}
			// A real later status save removes old fields, including shared-file merges.
			loaded[0].Metadata["note"] = "changed"
			loaded[0].Status = cliproxyauth.StatusActive
			if _, err := store.Save(t.Context(), loaded[0]); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil || bytes.Contains(saved, []byte("codex_turn_ticket:")) || bytes.Contains(saved, []byte("legacy-cookie")) {
				t.Fatalf("old ticket returned on save: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(saved, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["access_token"] != "original-token" || decoded["note"] != "changed" {
				t.Fatal("unrelated credentials changed")
			}
		})
	}
}

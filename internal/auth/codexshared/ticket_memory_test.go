package codexshared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedTokenRefreshDoesNotRetainLegacyTicket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	raw := `{"access_token":"old","codex_cli":{"enabled":true},"note":"keep","codex_turn_ticket:model":{"state":"old-ticket","route":"old-cookies"}}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveTokens(path, map[string]any{"access_token": "new"}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "codex_turn_ticket:") || strings.Contains(string(saved), "old-ticket") || strings.Contains(string(saved), "old-cookies") {
		t.Fatal("token refresh rewrote a legacy ticket")
	}
	metadata, err := Read(path)
	if err != nil || metadata["access_token"] != "new" || metadata["note"] != "keep" {
		t.Fatalf("credential values changed: %v", err)
	}
	if state, ok := Get(metadata); !ok || !state.Enabled {
		t.Fatal("runtime control flags changed")
	}
}

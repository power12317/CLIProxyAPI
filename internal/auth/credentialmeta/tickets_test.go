package credentialmeta

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanLegacyTicketsOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.json")
	raw := []byte(`{"type":"codex","access_token":"keep","large":9007199254740993,"codex_cli":{"enabled":false},"codex_cookie_preserve_oailb":true,"codex_turn_ticket:gpt-6-astra":{"state":"old-ticket","route":{"cflb":"old-cookie","oailb":"old-cookie"}}}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cleaned, err := CleanLegacyTurnTicketFiles(dir)
	if err != nil || cleaned != 1 {
		t.Fatalf("cleanup count=%d error=%v", cleaned, err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte("old-ticket")) || bytes.Contains(saved, []byte("old-cookie")) || bytes.Contains(saved, []byte(TurnTicketPrefix)) {
		t.Fatal("legacy material retained")
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(saved, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, TurnTicketPrefix+"gpt-6-astra")
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("non-ticket fields changed")
	}
	marker := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, marker, marker); err != nil {
		t.Fatal(err)
	}
	cleaned, err = CleanLegacyTurnTicketFiles(dir)
	if err != nil || cleaned != 0 {
		t.Fatalf("repeat cleanup count=%d error=%v", cleaned, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(marker) {
		t.Fatal("clean credential rewritten")
	}
}

func TestTurnTicketFilterDoesNotMutateSource(t *testing.T) {
	metadata := map[string]any{"access_token": "keep", TurnTicketPrefix + "model": map[string]any{"state": "secret"}, "codex_cookie_preserve_oailb": true}
	filtered := WithoutTurnTickets(metadata)
	if len(metadata) != 3 || len(filtered) != 2 || filtered["access_token"] != "keep" || filtered["codex_cookie_preserve_oailb"] != true {
		t.Fatal("filter changed unrelated metadata")
	}
}

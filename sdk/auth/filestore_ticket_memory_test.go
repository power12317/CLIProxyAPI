package auth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCredentialSavesExcludeTurnTickets(t *testing.T) {
	for _, kind := range []string{"metadata", "shared", "storage", "direct-storage", "shared-refresh"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "synthetic.json")
			meta := map[string]any{"type": "codex", "access_token": "old", "codex_cookie_preserve_oailb": true, "codex_turn_ticket:model": map[string]any{"state": "secret-ticket", "route": "secret-cookie"}}
			store := NewFileTokenStore()
			store.SetBaseDir(dir)
			auth := &coreauth.Auth{ID: "synthetic.json", FileName: "synthetic.json", Provider: "codex", Metadata: meta}
			if kind == "shared" || kind == "shared-refresh" {
				if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"old","codex_cli":{"enabled":false},"codex_turn_ticket:disk":{"state":"disk-secret"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			token := &codexauth.CodexTokenStorage{AccessToken: "new", Metadata: meta}
			var err error
			switch kind {
			case "direct-storage":
				err = token.SaveTokenToFile(path)
			case "shared-refresh":
				_, err = codexshared.SaveTokens(path, map[string]any{"access_token": "new"})
			default:
				if kind == "storage" {
					auth.Storage = token
				}
				_, err = store.Save(context.Background(), auth)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"codex_turn_ticket:", "secret-ticket", "secret-cookie", "disk-secret"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("%s persisted runtime ticket data", kind)
				}
			}
		})
	}
}

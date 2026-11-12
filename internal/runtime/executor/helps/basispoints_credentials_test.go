package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func officeCredentialFixture(expiry int64, signature string) map[string]any {
	raw, _ := json.Marshal(map[string]any{"client_id": codex.BasispointsClientID, "aud": codex.BasispointsClientID, "exp": expiry, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account"}})
	return map[string]any{"client_id": codex.BasispointsClientID, "access_token": "header." + base64.RawURLEncoding.EncodeToString(raw) + "." + signature, "id_token": "header." + base64.RawURLEncoding.EncodeToString(raw) + "." + signature, "refresh_token": "office-refresh", "account_id": "account", "expired": time.Unix(expiry, 0).UTC().Format(time.RFC3339)}
}

func TestBasispointsRefreshPersistsOnlyOfficeAndSerializesRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	metadata := map[string]any{"type": "codex", "access_token": "outer", "refresh_token": "outer-refresh", "account_id": "account", "siwc": map[string]any{"access_token": "siwc"}, "basispoints": officeCredentialFixture(1, "expired"), "unknown_counter": json.Number("9007199254740993")}
	if err := codex.WriteJSONFileAtomic(path, metadata); err != nil {
		t.Fatal(err)
	}
	auth := &coreauth.Auth{ID: "account", Provider: "codex", Metadata: metadata}
	var refreshes atomic.Int32
	refresh := func(_ context.Context, current map[string]any) (*codex.BasispointsTokenData, error) {
		refreshes.Add(1)
		if current["refresh_token"] != "office-refresh" {
			t.Error("used another credential partition")
		}
		return &codex.BasispointsTokenData{CodexSIWCTokenData: codex.CodexSIWCTokenData{AccessToken: officeCredentialFixture(4102444800, "rotated")["access_token"].(string), IDToken: officeCredentialFixture(4102444800, "identity")["id_token"].(string), RefreshToken: "rotated-refresh", ClientID: codex.BasispointsClientID, Expire: "2100-01-01T00:00:00Z"}, AccountID: "account"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, account, err := resolveBasispointsCredentials(t.Context(), auth, path, refresh)
			if err != nil || account != "account" || !strings.HasSuffix(token, ".rotated") {
				t.Errorf("Office resolution failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refresh calls=%d", refreshes.Load())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("Office refresh rounded unrelated JSON metadata")
	}
	var saved map[string]any
	_ = json.Unmarshal(raw, &saved)
	if saved["access_token"] != "outer" || saved["refresh_token"] != "outer-refresh" || saved["siwc"].(map[string]any)["access_token"] != "siwc" || saved["basispoints"].(map[string]any)["refresh_token"] != "rotated-refresh" {
		t.Fatal("Office refresh changed another credential partition")
	}
	if metadata["basispoints"].(map[string]any)["refresh_token"] != "office-refresh" {
		t.Fatal("mutated caller's stale auth snapshot")
	}
}

func TestBasispointsRefreshFailureDoesNotPublishOrPoisonCodex(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "account.json")
			metadata := map[string]any{"access_token": "outer", "account_id": "account", "basispoints": officeCredentialFixture(1, "expired")}
			if err := codex.WriteJSONFileAtomic(path, metadata); err != nil {
				t.Fatal(err)
			}
			auth := &coreauth.Auth{Metadata: metadata}
			token, _, err := resolveBasispointsCredentials(t.Context(), auth, path, func(context.Context, map[string]any) (*codex.BasispointsTokenData, error) {
				return nil, &codex.BasispointsTokenError{Status: 400, ReauthorizationRequired: terminal}
			})
			if err == nil || token != "" {
				t.Fatal("failed refresh returned credentials")
			}
			raw, _ := os.ReadFile(path)
			var saved map[string]any
			_ = json.Unmarshal(raw, &saved)
			if saved["access_token"] != "outer" {
				t.Fatal("Office failure changed Codex")
			}
			_, marked := saved["basispoints"].(map[string]any)["error"]
			if marked != terminal {
				t.Fatal("wrong Office reauthorization status")
			}
		})
	}
}

func TestBasispointsRejectedTokenDoesNotInvalidateRotatedGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	grant := officeCredentialFixture(4102444800, "rotated")
	metadata := map[string]any{"basispoints": grant}
	if err := codex.WriteJSONFileAtomic(path, metadata); err != nil {
		t.Fatal(err)
	}
	if err := RejectBasispointsAccessToken(path, "old-access"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var saved map[string]any
	_ = json.Unmarshal(raw, &saved)
	if saved["basispoints"].(map[string]any)["_access_token_rejected"] != nil {
		t.Fatal("rejected a concurrently rotated grant")
	}
	if err := RejectBasispointsAccessToken(path, grant["access_token"].(string)); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	_ = json.Unmarshal(raw, &saved)
	if saved["basispoints"].(map[string]any)["_access_token_rejected"] != true {
		t.Fatal("did not record rejected Office access token")
	}
}

package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestV8CodexClientSIWCPreferencePersistsAndResets(t *testing.T) {
	f := setupSIWCFlow(t, false)
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(filepath.Dir(f.path))
	f.manager.SetStore(store)
	f.router.PATCH("/v8/management/credentials/fields", f.h.PatchAuthFileFields)
	for _, value := range []any{false, true, false, nil} {
		body := map[string]any{"name": f.auth.FileName, "use_siwc": true, "allow_codex_client_siwc": value}
		w := siwcRequest(t, f.router, http.MethodPatch, "/v8/management/credentials/fields", body)
		if w.Code != http.StatusOK {
			t.Fatalf("patch=%d: %s", w.Code, w.Body.String())
		}
		current, ok := f.manager.GetByID(f.auth.ID)
		if !ok {
			t.Fatal("credential disappeared")
		}
		want := value != false
		if coreauth.CodexClientSIWCAllowed(current) != want || current.Metadata["use_siwc"] != true {
			t.Fatal("in-memory preference mismatch")
		}
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		var disk map[string]any
		if err := json.Unmarshal(raw, &disk); err != nil {
			t.Fatal(err)
		}
		if coreauth.CodexClientSIWCAllowed(&coreauth.Auth{Metadata: disk}) != want {
			t.Fatal("saved preference mismatch")
		}
		if disk["access_token"] != "outer" || disk["note"] != "keep" || disk["siwc"] == nil {
			t.Fatal("preference save lost credential metadata")
		}
		// Ordinary token snapshots must not restore a stale switch value, including after deletion.
		merged := codex.MergeWithDiskMap(disk, map[string]any{"allow_codex_client_siwc": !want})
		if coreauth.CodexClientSIWCAllowed(&coreauth.Auth{Metadata: merged}) != want {
			t.Fatal("stale token save replaced the switch")
		}
		if _, err := store.Save(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
}

package synthesizer

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexRuntimeFileCredentialIdentitySurvivesModeChanges(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join("team", "原有 account.json")
	path := filepath.Join(dir, name)
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: dir, Now: time.Unix(1234, 0)}
	var initial *coreauth.Auth
	for _, enabled := range []bool{false, true, false} {
		ctx.Config.Codex.Runtime.Enabled = enabled
		data := []byte(fmt.Sprintf(`{"type":"codex","email":"account@example.com","access_token":"synthetic","refresh_token":"synthetic","account_id":"account","prefix":"existing","codex_cli":{"enabled":%t}}`, enabled))
		auths, err := SynthesizeAuthFile(ctx, path, data)
		if err != nil || len(auths) != 1 {
			t.Fatalf("enabled=%t: synthesis error %v, count %d", enabled, err, len(auths))
		}
		auth := auths[0]
		if auth.ID != name || auth.Provider != "codex" || auth.Attributes[coreauth.AttributePath] != path {
			t.Fatalf("enabled=%t: original credential identity changed: %+v", enabled, auth)
		}
		if initial == nil {
			initial = auth
			continue
		}
		if auth.FileName != initial.FileName || auth.Prefix != initial.Prefix || auth.Label != initial.Label || auth.Index != initial.Index || !reflect.DeepEqual(auth.Attributes, initial.Attributes) {
			t.Fatalf("enabled=%t: credential routing metadata changed: before=%+v after=%+v", enabled, initial, auth)
		}
	}
}

func TestLegacyCodexRuntimeFileCredentialIgnored(t *testing.T) {
	ctx := &SynthesisContext{Config: &config.Config{}, Now: time.Unix(1234, 0)}
	for _, provider := range []string{"codex-runtime", " CODEX-RUNTIME "} {
		auths, err := SynthesizeAuthFile(ctx, filepath.Join(t.TempDir(), "legacy.json"), []byte(`{"type":"`+provider+`","access_token":"synthetic","refresh_token":"synthetic"}`))
		if err != nil || len(auths) != 0 {
			t.Fatalf("legacy provider %q produced file credentials: %v, %v", provider, auths, err)
		}
	}
}

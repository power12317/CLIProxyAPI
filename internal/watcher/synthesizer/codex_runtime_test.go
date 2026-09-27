package synthesizer

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexRuntimeSynthesisAndReload(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true, Workers: []config.CodexRuntimeWorker{{ID: "worker", Socket: "/run/worker.sock", AccountID: "account", Models: []string{"model"}, Prefix: "runtime"}}}}}
	ctx := &SynthesisContext{Config: cfg, Now: time.Unix(1234, 0), IDGenerator: NewStableIDGenerator()}
	s := NewConfigSynthesizer()
	auths, err := s.Synthesize(ctx)
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesis: %v, count %d", err, len(auths))
	}
	a := auths[0]
	if !coreauth.IsCodexRuntimeAuth(a) || a.ID != "codex-runtime:worker" || a.Prefix != "runtime" || a.Metadata != nil || a.FileName != "" || a.Attributes["api_key"] != "" {
		t.Fatalf("unexpected runtime reference: %+v", a)
	}
	w := &cfg.Codex.Runtime.Workers[0]
	w.Socket = "/run/new.sock"
	w.AccountID = "new-account"
	w.Models = []string{"new-model"}
	w.Disabled = true
	updated, err := s.Synthesize(ctx)
	if err != nil || len(updated) != 1 {
		t.Fatal(err)
	}
	u := updated[0]
	if u.ID != a.ID || !u.Disabled || u.Status != coreauth.StatusDisabled || reflect.DeepEqual(u.Attributes, a.Attributes) || u.Attributes["models_hash"] == a.Attributes["models_hash"] {
		t.Fatal("configuration changes were not observable")
	}
	cfg.Codex.Runtime.Enabled = false
	removed, err := s.Synthesize(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatal("disabled runtime produced references")
	}
	for _, provider := range []string{"codex-runtime", " CODEX-RUNTIME "} {
		injected, err := SynthesizeAuthFile(ctx, "/tmp/forged.json", []byte(`{"type":"`+provider+`","access_token":"synthetic","refresh_token":"synthetic"}`))
		if err != nil || len(injected) != 0 {
			t.Fatal("file-based runtime credentials accepted")
		}
	}
}

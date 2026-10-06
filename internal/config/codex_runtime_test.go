package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexRuntimeUsesSingleDefaultMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("codex:\n  runtime:\n    enabled: true\n    credential-modes:\n      'team/中文 空格.json': false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.Runtime.Endpoint() != DefaultCodexRuntimeURL {
		t.Fatal(cfg.Codex.Runtime)
	}
	cloned := cfg.CloneForRuntime()
	cloned.Codex.Runtime.CredentialModes["team/中文 空格.json"] = true
	if cfg.Codex.Runtime.CredentialModes["team/中文 空格.json"] {
		t.Fatal("preference map is shared across config snapshots")
	}
}

func TestValidateCodexRuntime(t *testing.T) {
	cfg := &Config{Codex: CodexConfig{Runtime: CodexRuntimeConfig{Enabled: true}}}
	if err := cfg.ValidateCodexRuntime(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://localhost", "ws:///missing", "://"} {
		cfg.Codex.Runtime.URL = endpoint
		if cfg.ValidateCodexRuntime() == nil {
			t.Fatal("invalid endpoint accepted", endpoint)
		}
	}
	cfg.Codex.Runtime.URL = ""
	cfg.Home.Enabled = true
	if cfg.ValidateCodexRuntime() == nil {
		t.Fatal("Home/runtime combination accepted")
	}
	cfg.Codex.Runtime.Enabled = false
	if err := cfg.ValidateCodexRuntime(); err != nil {
		t.Fatal(err)
	}
}

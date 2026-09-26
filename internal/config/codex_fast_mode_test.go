package config

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestCodexFastModeConfiguration(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"", ""}, {"auto", "auto"}, {"default", "default"}, {"fast", "fast"}, {"ultrafast", "ultrafast"}, {" FAST ", "fast"}, {"unsupported", "auto"}} {
		t.Run(tc.input, func(t *testing.T) {
			raw := []byte(fmt.Sprintf("codex-header-defaults:\n  user-agent: test-agent\n  beta-features: test-beta\n  fast-mode: %q\n", tc.input))
			cfg, err := ParseConfigBytes(raw)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CodexHeaderDefaults.FastMode != tc.want {
				t.Fatalf("mode=%q want=%q", cfg.CodexHeaderDefaults.FastMode, tc.want)
			}
			if cfg.CodexHeaderDefaults.UserAgent != "test-agent" || cfg.CodexHeaderDefaults.BetaFeatures != "test-beta" {
				t.Fatal("header defaults changed")
			}
			encoded, err := json.Marshal(cfg.CodexHeaderDefaults)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]string
			if err = json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["fast-mode"] != tc.want {
				t.Fatalf("management JSON mode=%q", decoded["fast-mode"])
			}
		})
	}
	cfg, err := ParseConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexHeaderDefaults.FastMode != "" {
		t.Fatal("missing mode must not add an override")
	}
}

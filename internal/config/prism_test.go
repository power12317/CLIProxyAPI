package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrismSingleSwitchDefaultsAndSerialization(t *testing.T) {
	for _, tc := range []struct {
		yaml    string
		enabled bool
	}{
		{"codex: {}\n", false},
		{"codex:\n  prism:\n    enabled: true\n", true},
		{"codex:\n  prism:\n    enabled: false\n", false},
	} {
		file := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(file, []byte(tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Codex.Prism.Enabled != tc.enabled {
			t.Fatalf("enabled = %v, want %v", cfg.Codex.Prism.Enabled, tc.enabled)
		}
		data, err := json.Marshal(cfg.Codex.Prism)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 1 || fields["enabled"] != tc.enabled {
			t.Fatalf("expected only the switch: %s", data)
		}
	}
}

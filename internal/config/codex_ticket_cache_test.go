package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTicketCacheAllModelsDefaultAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"omitted", "codex:\n  turn-state-ticket:\n    enabled: true\n", true},
		{"enabled", "codex:\n  turn-state-ticket:\n    enabled: true\n    cache-all-models: true\n", true},
		{"disabled", "codex:\n  turn-state-ticket:\n    enabled: true\n    cache-all-models: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tc.yaml))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := cfg.Codex.EffectiveTurnStateTicket().CacheAllModelsEnabled(); got != tc.want {
				t.Fatalf("cache all models = %v, want %v", got, tc.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, []byte(tc.yaml), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
				t.Fatal(errSave)
			}
			reloaded, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if reloaded.Codex.TurnStateTicket.CacheAllModelsEnabled() != tc.want {
				t.Fatal("saving and reloading changed cache-all-models")
			}
			data, errMarshal := json.Marshal(cfg)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var decoded Config
			if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
				t.Fatal(errDecode)
			}
			if decoded.Codex.TurnStateTicket.CacheAllModelsEnabled() != tc.want {
				t.Fatal("JSON round trip changed cache-all-models")
			}
		})
	}
}

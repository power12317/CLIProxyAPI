package config

import (
	"encoding/json"
	"fmt"
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

func TestCodexTicketLegacyTargetLengthUsesFixedDefault(t *testing.T) {
	for _, length := range []int{0, 292, 332, 780} {
		cfg, errParse := ParseConfigBytes([]byte(fmt.Sprintf("codex:\n  turn-state-ticket:\n    enabled: true\n    target-length: %d\n", length)))
		if errParse != nil {
			t.Fatal(errParse)
		}
		policy := cfg.Codex.EffectiveTurnStateTicket()
		if policy.TargetLength != 780 || policy.TTLSeconds != 3600 || policy.RefreshBeforeSeconds != 600 || policy.ProbeIntervalSeconds != 60 || !policy.CacheAllModelsEnabled() {
			t.Fatalf("legacy length %d changed the retention policy: %+v", length, policy)
		}
	}
}

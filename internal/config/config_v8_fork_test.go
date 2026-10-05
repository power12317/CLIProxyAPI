package config

import (
	"testing"
)

func TestV8MigrationPreservesIndependentForkModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		enabled func(*Config) bool
	}{
		{"prism", "codex:\n  prism:\n    enabled: true\n", func(c *Config) bool { return c.Codex.Prism.Enabled }},
		{"basispoints", "codex:\n  basispoints:\n    enabled: true\n", func(c *Config) bool { return c.Codex.Basispoints.Enabled }},
		{"ticket", "codex:\n  turn-state-ticket:\n    enabled: true\n", func(c *Config) bool { return c.Codex.TurnStateTicket.Enabled }},
		{"websocket", "codex:\n  force-websocket: true\n", func(c *Config) bool { return c.Codex.ForceWebsocket }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, errParse := ParseConfigBytes([]byte(tc.yaml))
			if errParse != nil || !tc.enabled(legacy) {
				t.Fatalf("legacy setting lost: config=%+v error=%v", legacy, errParse)
			}
			migrated, changed, errNormalize := NormalizeConfigLayout([]byte(tc.yaml), true)
			if errNormalize != nil || !changed {
				t.Fatalf("migration failed: changed=%t error=%v", changed, errNormalize)
			}
			if errValidate := ValidateV8Config(migrated); errValidate != nil {
				t.Fatalf("fork setting rejected by v8 validation: %v", errValidate)
			}
			current, errParse := ParseConfigBytes(migrated)
			if errParse != nil || !tc.enabled(current) {
				t.Fatalf("migrated setting lost: config=%+v error=%v", current, errParse)
			}
		})
	}
}

func TestV8ForkHeadersAndTicketDetails(t *testing.T) {
	raw := []byte("config-version: 8\noauth:\n  providers:\n    codex:\n      device-convergence: false\n      header-defaults:\n        fast-mode: ultrafast\n      turn-state-ticket:\n        enabled: false\n        ttl-seconds: 2400\n")
	cfg, errParse := ParseConfigBytes(raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.Codex.DeviceConvergenceEnabled() || cfg.CodexHeaderDefaults.FastMode != "ultrafast" || cfg.Codex.TurnStateTicket.TTLSeconds != 2400 {
		t.Fatalf("fork v8 settings did not reach the runtime: %+v", cfg.Codex)
	}
}

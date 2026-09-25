package config

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func TestBasispointsConfigRoundTripAndTicketIsolation(t *testing.T) {
	var cfg Config
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if value := gjson.GetBytes(raw, "codex.basispoints.enabled"); !value.Exists() || value.Bool() {
		t.Fatal("default capability is not exposed as false")
	}
	if err = yaml.Unmarshal([]byte("codex:\n  basispoints:\n    enabled: true\n  force-websocket: true\n  turn-state-ticket:\n    enabled: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Codex.Basispoints.Enabled || !cfg.Codex.ForceWebsocket {
		t.Fatal("configuration was not parsed")
	}
	if cfg.Codex.EffectiveTurnStateTicket().Enabled {
		t.Fatal("native ticket harvester remains active")
	}
	encoded, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err = yaml.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Codex.Basispoints.Enabled || !saved.Codex.TurnStateTicket.Enabled {
		t.Fatal("round trip lost configuration")
	}
	saved.Codex.Basispoints.Enabled = false
	if !saved.Codex.EffectiveTurnStateTicket().Enabled {
		t.Fatal("original ticket setting not restored")
	}
}

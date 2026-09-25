package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

func TestEffectiveSDKConfigBasispointsOverridesNativeTransport(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{
		Basispoints:    config.CodexBasispointsConfig{Enabled: true},
		ForceWebsocket: true, ResponseSteering: true,
	}}
	actual := effectiveSDKConfig(cfg)
	if !actual.CodexBasispoints || actual.CodexForceWebsocket || actual.CodexResponseSteering {
		t.Fatalf("unexpected effective transport: %+v", actual)
	}
	if !cfg.Codex.ForceWebsocket || !cfg.Codex.ResponseSteering {
		t.Fatal("stored native settings were modified")
	}
}

func TestEffectiveSDKConfigCopiesCodexOptimizeMultiAgentV2(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}}

	sdkCfg := effectiveSDKConfig(cfg)
	if sdkCfg == nil || !sdkCfg.CodexOptimizeMultiAgentV2 {
		t.Fatalf("CodexOptimizeMultiAgentV2 = false, want true")
	}
}

func TestEffectiveSDKConfigCopiesCodexOrphanDelegationCompatibility(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OrphanDelegationCompatibility: true}}

	sdkCfg := effectiveSDKConfig(cfg)
	if sdkCfg == nil || !sdkCfg.CodexOrphanDelegationCompatibility {
		t.Fatalf("CodexOrphanDelegationCompatibility = false, want true")
	}
}

func TestEffectiveSDKConfigCopiesCodexResponseSteering(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{ResponseSteering: true}}

	sdkCfg := effectiveSDKConfig(cfg)
	if sdkCfg == nil || !sdkCfg.CodexResponseSteering {
		t.Fatalf("CodexResponseSteering = false, want true")
	}
}

func TestCodexResponseSteeringYAMLUnmarshal(t *testing.T) {
	yamlContent := []byte(`
codex:
  response-steering: true
`)
	var cfg config.Config
	if err := yaml.Unmarshal(yamlContent, &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !cfg.Codex.ResponseSteering {
		t.Fatalf("cfg.Codex.ResponseSteering = false, want true")
	}
	sdkCfg := effectiveSDKConfig(&cfg)
	if sdkCfg == nil || !sdkCfg.CodexResponseSteering {
		t.Fatalf("sdkCfg.CodexResponseSteering = false, want true")
	}
}

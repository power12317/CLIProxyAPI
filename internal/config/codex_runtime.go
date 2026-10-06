package config

import (
	"fmt"
	"net/url"
)

const DefaultCodexRuntimeURL = "ws://127.0.0.1:38317/cpa/v1/ws"

// CodexRuntimeConfig selects the master and remembers per-credential preferences.
type CodexRuntimeConfig struct {
	Enabled         bool            `yaml:"enabled" json:"enabled"`
	URL             string          `yaml:"url,omitempty" json:"url,omitempty"`
	CredentialModes map[string]bool `yaml:"credential-modes,omitempty" json:"credential_modes,omitempty"`
}

func (c CodexRuntimeConfig) Endpoint() string {
	if c.URL == "" {
		return DefaultCodexRuntimeURL
	}
	return c.URL
}

func (cfg *Config) ValidateCodexRuntime() error {
	if cfg == nil || !cfg.Codex.Runtime.Enabled {
		return nil
	}
	if cfg.Home.Enabled {
		return fmt.Errorf("codex.runtime is not supported in Home mode")
	}
	u, err := url.Parse(cfg.Codex.Runtime.Endpoint())
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("codex.runtime.url must be a WebSocket URL")
	}
	return nil
}

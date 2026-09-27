package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// CodexRuntimeConfig references separately deployed managed-auth Codex workers.
type CodexRuntimeConfig struct {
	Enabled bool                 `yaml:"enabled" json:"enabled"`
	Workers []CodexRuntimeWorker `yaml:"workers" json:"workers"`
}

// CodexRuntimeWorker contains routing metadata only, never OAuth credentials.
type CodexRuntimeWorker struct {
	ID        string   `yaml:"id" json:"id"`
	URL       string   `yaml:"url,omitempty" json:"url,omitempty"`
	Token     string   `yaml:"token,omitempty" json:"token,omitempty"`
	AuthFile  string   `yaml:"auth-file,omitempty" json:"auth_file,omitempty"`
	Socket    string   `yaml:"socket,omitempty" json:"socket,omitempty"`
	AccountID string   `yaml:"account-id,omitempty" json:"account-id,omitempty"`
	Models    []string `yaml:"models" json:"models"`
	Prefix    string   `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Disabled  bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

func (cfg *Config) ValidateCodexRuntime() error {
	if cfg == nil || !cfg.Codex.Runtime.Enabled {
		return nil
	}
	if cfg.Home.Enabled {
		return fmt.Errorf("codex.runtime is not supported in Home mode")
	}
	ids, sockets := map[string]bool{}, map[string]bool{}
	for i, w := range cfg.Codex.Runtime.Workers {
		id := strings.TrimSpace(w.ID)
		if id == "" || id != w.ID || ids[id] {
			return fmt.Errorf("codex.runtime.workers[%d]: id must be nonempty and unique", i)
		}
		if w.URL != "" {
			u, err := url.Parse(w.URL)
			if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
				return fmt.Errorf("codex.runtime.workers[%d]: url must be a WebSocket URL", i)
			}
			if w.AuthFile == "" || filepath.Base(w.AuthFile) != w.AuthFile || filepath.Ext(w.AuthFile) != ".json" {
				return fmt.Errorf("codex.runtime.workers[%d]: auth-file must be a JSON filename", i)
			}
			if sockets[w.URL] || sockets["file:"+w.AuthFile] {
				return fmt.Errorf("codex.runtime.workers[%d]: duplicate URL or auth-file", i)
			}
			ids[id], sockets[w.URL], sockets["file:"+w.AuthFile] = true, true, true
			continue
		}
		if !filepath.IsAbs(w.Socket) || filepath.Clean(w.Socket) != w.Socket || sockets[w.Socket] {
			return fmt.Errorf("codex.runtime.workers[%d]: socket must be a unique absolute normalized path", i)
		}
		if strings.TrimSpace(w.AccountID) == "" || strings.TrimSpace(w.AccountID) != w.AccountID {
			return fmt.Errorf("codex.runtime.workers[%d]: account-id is required", i)
		}
		if strings.Contains(w.Prefix, "/") || strings.TrimSpace(w.Prefix) != w.Prefix {
			return fmt.Errorf("codex.runtime.workers[%d]: invalid prefix", i)
		}
		if len(w.Models) == 0 {
			return fmt.Errorf("codex.runtime.workers[%d]: models must be explicit", i)
		}
		seen := map[string]bool{}
		for _, model := range w.Models {
			if strings.TrimSpace(model) == "" || strings.TrimSpace(model) != model || seen[model] {
				return fmt.Errorf("codex.runtime.workers[%d]: models must be nonempty and unique", i)
			}
			seen[model] = true
		}
		ids[id], sockets[w.Socket] = true, true
	}
	return nil
}

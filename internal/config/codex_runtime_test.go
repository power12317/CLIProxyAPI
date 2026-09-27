package config

import "testing"

func TestValidateCodexRuntime(t *testing.T) {
	valid := func() *Config {
		return &Config{Codex: CodexConfig{Runtime: CodexRuntimeConfig{Enabled: true, Workers: []CodexRuntimeWorker{{ID: "worker", Socket: "/run/codex/worker.sock", AccountID: "account", Models: []string{"model"}, Prefix: "runtime"}}}}}
	}
	if err := valid().ValidateCodexRuntime(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"home": func(c *Config) { c.Home.Enabled = true },
		"duplicate-id": func(c *Config) {
			w := c.Codex.Runtime.Workers[0]
			w.Socket = "/run/other.sock"
			c.Codex.Runtime.Workers = append(c.Codex.Runtime.Workers, w)
		},
		"duplicate-socket": func(c *Config) {
			w := c.Codex.Runtime.Workers[0]
			w.ID = "other"
			c.Codex.Runtime.Workers = append(c.Codex.Runtime.Workers, w)
		},
		"relative-socket":     func(c *Config) { c.Codex.Runtime.Workers[0].Socket = "relative.sock" },
		"unnormalized-socket": func(c *Config) { c.Codex.Runtime.Workers[0].Socket = "/run/../run/codex.sock" },
		"missing-account":     func(c *Config) { c.Codex.Runtime.Workers[0].AccountID = "" },
		"missing-models":      func(c *Config) { c.Codex.Runtime.Workers[0].Models = nil },
		"duplicate-model":     func(c *Config) { c.Codex.Runtime.Workers[0].Models = []string{"model", "model"} },
		"invalid-prefix":      func(c *Config) { c.Codex.Runtime.Workers[0].Prefix = "one/two" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(cfg)
			if cfg.ValidateCodexRuntime() == nil {
				t.Fatal("invalid runtime accepted")
			}
		})
	}
	cfg := valid()
	cfg.Codex.Runtime.Enabled = false
	cfg.Home.Enabled = true
	if err := cfg.ValidateCodexRuntime(); err != nil {
		t.Fatal("disabled runtime affected existing config:", err)
	}
}

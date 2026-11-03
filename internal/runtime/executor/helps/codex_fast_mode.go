package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func codexFastModeTier(cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.CodexHeaderDefaults.FastMode)) {
	case "default":
		return "default", true
	case "fast":
		return "priority", true
	case "ultrafast":
		return "ultrafast", true
	default:
		return "", false
	}
}

// ApplyCodexFastMode applies an explicit operator override after translation and
// payload rules. Auto adds no override and never restores client fields.
func ApplyCodexFastMode(body []byte, cfg *config.Config) []byte {
	tier, override := codexFastModeTier(cfg)
	if !override {
		return body
	}
	var updated []byte
	var err error
	if tier == "default" {
		updated, err = ojson.DeleteBytes(body, "service_tier")
	} else {
		updated, err = ojson.SetBytes(body, "service_tier", tier)
	}
	if err != nil {
		return body
	}
	return updated
}

// SetCodexFastMode records the operator-selected request tier for monitoring.
// Auto retains the original client-requested tier and its existing fallback.
func (r *UsageReporter) SetCodexFastMode(cfg *config.Config) {
	if r == nil {
		return
	}
	if tier, override := codexFastModeTier(cfg); override {
		r.serviceTier = tier
	}
}

// ApplyCodexFastModeDoc applies the operator override before final payload rules.
func ApplyCodexFastModeDoc(doc *ojson.Doc, cfg *config.Config) {
	if doc == nil {
		return
	}
	tier, override := codexFastModeTier(cfg)
	if !override {
		return
	}
	if tier == "default" {
		_ = doc.Delete("service_tier")
	} else {
		_ = doc.Set("service_tier", tier)
	}
}

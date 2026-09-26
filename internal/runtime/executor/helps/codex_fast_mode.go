package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/sjson"
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
		updated, err = sjson.DeleteBytes(body, "service_tier")
	} else {
		updated, err = sjson.SetBytes(body, "service_tier", tier)
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

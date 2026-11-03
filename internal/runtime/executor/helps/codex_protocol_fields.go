package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// PreserveCodexProtocolFields carries explicit native intent through format translation.
// Payload configuration runs afterwards; outbound wire rules still apply.
func PreserveCodexProtocolFields(original, translated []byte, nativeHeaders ...bool) []byte {
	native := len(nativeHeaders) > 0 && nativeHeaders[0]
	if !native && !IsOfficialCodexRequest(original) && util.CodexResponsesLiteHeaderValue(original, nil) == "" && ojson.GetBytes(original, "reasoning.context").String() != "all_turns" {
		return NormalizeCodexServiceTier(translated)
	}
	for _, path := range []string{"client_metadata", "reasoning.context", "parallel_tool_calls"} {
		if value := ojson.GetBytes(original, path); value.Exists() {
			if updated, err := ojson.SetRawBytes(translated, path, []byte(value.Raw)); err == nil {
				translated = updated
			}
		}
	}
	// Include remains caller-owned. Service tiers come only from format
	// translation; never copy a tier back from the original native request.
	if value := ojson.GetBytes(original, "include"); value.Exists() {
		if updated, err := ojson.SetRawBytes(translated, "include", []byte(value.Raw)); err == nil {
			translated = updated
		}
	} else if updated, err := ojson.DeleteBytes(translated, "include"); err == nil {
		translated = updated
	}
	return NormalizeCodexServiceTier(translated)
}

// NormalizeCodexServiceTier normalizes client tiers before payload rules.
func NormalizeCodexServiceTier(body []byte) []byte {
	value := ojson.GetBytes(body, "service_tier")
	if !value.Exists() {
		return body
	}
	var tier string
	if value.Type == ojson.String {
		tier = codexwire.ServiceTier(value.String())
	}
	var updated []byte
	var err error
	if tier == "" {
		updated, err = ojson.DeleteBytes(body, "service_tier")
	} else if value.String() == tier {
		return body
	} else {
		updated, err = ojson.SetBytes(body, "service_tier", tier)
	}
	if err != nil {
		return body
	}
	return updated
}

// omitCodexDefaultServiceTier preserves native Codex omission semantics after
// payload rules without changing explicit accelerated or custom tier values.
func omitCodexDefaultServiceTier(body []byte) []byte {
	value := ojson.GetBytes(body, "service_tier")
	if value.Type != ojson.String {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(value.String())) {
	case "default", "auto":
		if updated, errDelete := ojson.DeleteBytes(body, "service_tier"); errDelete == nil {
			return updated
		}
	}
	return body
}

// omitCodexDefaultServiceTierDoc preserves the native omission of ordinary tiers.
func omitCodexDefaultServiceTierDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	value := doc.Get("service_tier")
	if value.Type != ojson.String {
		return
	}
	tier := strings.ToLower(strings.TrimSpace(value.String()))
	if tier == "default" || tier == "auto" {
		_ = doc.Delete("service_tier")
	}
}

// NormalizeCodexServiceTierDoc canonicalizes direct Codex tiers in place.
func NormalizeCodexServiceTierDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	value := doc.Get("service_tier")
	if !value.Exists() {
		return
	}
	tier := strings.ToLower(strings.TrimSpace(value.String()))
	if value.Type != ojson.String {
		_ = doc.Delete("service_tier")
		return
	}
	switch tier {
	case "fast", "priority":
		_ = doc.Set("service_tier", "priority")
	case "ultrafast":
		_ = doc.Set("service_tier", "ultrafast")
	default:
		_ = doc.Delete("service_tier")
	}
}

// NormalizeCodexRuntimeServiceTierDoc retains native values except the public fast alias.
func NormalizeCodexRuntimeServiceTierDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	if value := doc.Get("service_tier"); value.Type == ojson.String && strings.EqualFold(strings.TrimSpace(value.String()), "fast") {
		_ = doc.Set("service_tier", "priority")
	}
}

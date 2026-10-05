package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PreserveCodexProtocolFields carries explicit native intent through format translation.
// Payload configuration runs afterwards; outbound wire rules still apply.
func PreserveCodexProtocolFields(original, translated []byte, nativeHeaders ...bool) []byte {
	native := len(nativeHeaders) > 0 && nativeHeaders[0]
	if !native && !IsOfficialCodexRequest(original) && util.CodexResponsesLiteHeaderValue(original, nil) == "" && gjson.GetBytes(original, "reasoning.context").String() != "all_turns" {
		return NormalizeCodexServiceTier(translated)
	}
	for _, path := range []string{"client_metadata", "reasoning.context", "parallel_tool_calls"} {
		if value := gjson.GetBytes(original, path); value.Exists() {
			if updated, err := sjson.SetRawBytes(translated, path, []byte(value.Raw)); err == nil {
				translated = updated
			}
		}
	}
	// Include remains caller-owned. Service tiers come only from format
	// translation; never copy a tier back from the original native request.
	if value := gjson.GetBytes(original, "include"); value.Exists() {
		if updated, err := sjson.SetRawBytes(translated, "include", []byte(value.Raw)); err == nil {
			translated = updated
		}
	} else if updated, err := sjson.DeleteBytes(translated, "include"); err == nil {
		translated = updated
	}
	return NormalizeCodexServiceTier(translated)
}

// NormalizeCodexServiceTier normalizes client tiers before payload rules.
func NormalizeCodexServiceTier(body []byte) []byte {
	value := gjson.GetBytes(body, "service_tier")
	if !value.Exists() {
		return body
	}
	var tier string
	if value.Type == gjson.String {
		tier = codexwire.ServiceTier(value.String())
	}
	var updated []byte
	var err error
	if tier == "" {
		updated, err = sjson.DeleteBytes(body, "service_tier")
	} else if value.String() == tier {
		return body
	} else {
		updated, err = sjson.SetBytes(body, "service_tier", tier)
	}
	if err != nil {
		return body
	}
	return updated
}

// omitCodexDefaultServiceTier preserves native Codex omission semantics after
// payload rules without changing explicit accelerated or custom tier values.
func omitCodexDefaultServiceTier(body []byte) []byte {
	value := gjson.GetBytes(body, "service_tier")
	if value.Type != gjson.String {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(value.String())) {
	case "default", "auto":
		if updated, errDelete := sjson.DeleteBytes(body, "service_tier"); errDelete == nil {
			return updated
		}
	}
	return body
}

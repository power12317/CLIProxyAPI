package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PreserveCodexProtocolFields carries explicit native intent through format translation.
// Payload configuration still runs afterwards and retains its existing precedence.
func PreserveCodexProtocolFields(original, translated []byte, nativeHeaders ...bool) []byte {
	native := len(nativeHeaders) > 0 && nativeHeaders[0]
	if !native && !IsOfficialCodexRequest(original) && util.CodexResponsesLiteHeaderValue(original, nil) == "" && gjson.GetBytes(original, "reasoning.context").String() != "all_turns" {
		return translated
	}
	for _, path := range []string{"client_metadata", "reasoning.context", "parallel_tool_calls"} {
		if value := gjson.GetBytes(original, path); value.Exists() {
			if updated, err := sjson.SetRawBytes(translated, path, []byte(value.Raw)); err == nil {
				translated = updated
			}
		}
	}
	// These optional fields belong to the native caller. The compatibility
	// translator must neither replace explicit values nor supply missing ones.
	for _, path := range []string{"service_tier", "include"} {
		if value := gjson.GetBytes(original, path); value.Exists() {
			if updated, err := sjson.SetRawBytes(translated, path, []byte(value.Raw)); err == nil {
				translated = updated
			}
		} else if updated, err := sjson.DeleteBytes(translated, path); err == nil {
			translated = updated
		}
	}
	return translated
}

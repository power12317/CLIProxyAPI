package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PreserveCodexProtocolFields carries explicit Lite intent through format translation.
// Payload configuration still runs afterwards and retains its existing precedence.
func PreserveCodexProtocolFields(original, translated []byte) []byte {
	if !IsOfficialCodexRequest(original) && !util.IsCodexResponsesLiteRequest(original, nil) && gjson.GetBytes(original, "reasoning.context").String() != "all_turns" {
		return translated
	}
	for _, path := range []string{"client_metadata", "reasoning.context", "parallel_tool_calls"} {
		if value := gjson.GetBytes(original, path); value.Exists() {
			if updated, err := sjson.SetRawBytes(translated, path, []byte(value.Raw)); err == nil {
				translated = updated
			}
		}
	}
	return translated
}

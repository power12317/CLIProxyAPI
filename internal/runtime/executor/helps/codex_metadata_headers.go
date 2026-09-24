package helps

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// CodexTurnMetadataHeader leaves the unbounded tool inventory in the body,
// matching the native client's compact compatibility header representation.
func CodexTurnMetadataHeader(raw string) string {
	var metadata map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata == nil {
		return raw
	}
	delete(metadata, "tool_namespaces_info")
	encoded, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return raw
	}
	return string(encoded)
}

// RestoreCodexMetadataHeaders restores only documented body/header mirrors.
// Existing client headers and configured overrides take precedence.
func RestoreCodexMetadataHeaders(headers http.Header, body []byte) {
	if headers == nil {
		return
	}
	for _, key := range []string{"X-Codex-Parent-Thread-Id", "X-OpenAI-Subagent", "X-Codex-Window-Id", "X-Codex-Turn-Metadata"} {
		if headers.Get(key) != "" {
			continue
		}
		value := gjson.GetBytes(body, "client_metadata."+strings.ToLower(key))
		if value.Type != gjson.String || value.String() == "" {
			continue
		}
		raw := value.String()
		if key == "X-Codex-Turn-Metadata" {
			raw = CodexTurnMetadataHeader(raw)
		}
		headers.Set(key, raw)
	}
}

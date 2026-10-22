package helps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyCodexRuntimeImagePolicy applies the operator's hosted-image policy without
// inventing client tools or reinterpreting functions that happen to share names.
func ApplyCodexRuntimeImagePolicy(body []byte, strip bool) []byte {
	if !strip {
		return body
	}
	var filter func(gjson.Result) ([]byte, bool)
	filter = func(list gjson.Result) ([]byte, bool) {
		if !list.IsArray() {
			return nil, false
		}
		changed := false
		kept := make([]json.RawMessage, 0, len(list.Array()))
		for _, tool := range list.Array() {
			if tool.Get("type").String() == "image_generation" {
				changed = true
				continue
			}
			raw := []byte(tool.Raw)
			if tool.Get("type").String() == "namespace" {
				if children, updated := filter(tool.Get("tools")); updated {
					raw, _ = sjson.SetRawBytes(raw, "tools", children)
					changed = true
				}
			}
			kept = append(kept, raw)
		}
		raw, _ := json.Marshal(kept)
		return raw, changed
	}
	return transformCodexToolLists(body, filter)
}

// NormalizeCodexRuntimeServiceTier maps a public alias while leaving other
// explicit business values for native capability mapping.
func NormalizeCodexRuntimeServiceTier(body []byte) []byte {
	value := gjson.GetBytes(body, "service_tier")
	if value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "fast") {
		body, _ = sjson.SetBytes(body, "service_tier", "priority")
	}
	return body
}

// ValidateCodexRuntimeChatCalls checks relationships before Chat translation can
// discard an orphan result or choose one of several missing call identifiers.
func ValidateCodexRuntimeChatCalls(body []byte) error {
	type pendingCall struct {
		id       string
		consumed bool
	}
	var pending []pendingCall
	for index, message := range gjson.GetBytes(body, "messages").Array() {
		if message.Get("role").String() != "tool" {
			pending = nil
			seen := map[string]bool{}
			for callIndex, call := range message.Get("tool_calls").Array() {
				kind := call.Get("type").String()
				if kind == "" && call.Get("function").IsObject() {
					kind = "function"
				}
				if kind != "function" && kind != "custom" {
					return fmt.Errorf("messages[%d].tool_calls[%d].type has no Chat tool-call mapping", index, callIndex)
				}
				if strings.TrimSpace(call.Get(kind+".name").String()) == "" {
					return fmt.Errorf("messages[%d].tool_calls[%d].%s.name is required", index, callIndex, kind)
				}
				id := call.Get("id").String()
				if id != "" && seen[id] {
					return fmt.Errorf("messages[%d].tool_calls[%d].id is ambiguous", index, callIndex)
				}
				seen[id] = true
				pending = append(pending, pendingCall{id: id})
			}
			continue
		}
		id := message.Get("tool_call_id").String()
		match, remaining := -1, 0
		for callIndex, call := range pending {
			if !call.consumed {
				remaining++
				if id == "" || call.id == id {
					match = callIndex
				}
			}
		}
		if id == "" && remaining != 1 {
			return fmt.Errorf("messages[%d].tool_call_id cannot be inferred from %d pending calls", index, remaining)
		}
		if match < 0 {
			return fmt.Errorf("messages[%d].tool_call_id has no matching tool call", index)
		}
		pending[match].consumed = true
	}
	return nil
}

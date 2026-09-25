package basispoints

import "strings"

// delegationContext represents a cross-thread handoff as context, not a result
// for a tool call in this conversation. The original output stays byte-exact.
func delegationContext(item object) (object, bool) {
	if item["type"] != "function_call_output" || stringValue(item["call_id"]) != "" || item["namespace"] != "codex_app" {
		return nil, false
	}
	name := stringValue(item["name"])
	if name != "create_thread" && name != "send_message_to_thread" {
		return nil, false
	}
	output, ok := item["output"].(string)
	if !ok || !strings.HasPrefix(strings.TrimSpace(output), "<codex_delegation>") {
		return nil, false
	}
	return message("user", "Tool output from codex_app__"+name+":\n"+output), true
}

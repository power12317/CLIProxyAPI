package basispoints

import "github.com/tidwall/gjson"

// NativeToolReason keeps hosted capabilities on the Codex channel instead of
// deleting declarations that the Excel transport cannot execute.
func NativeToolReason(raw []byte) string {
	var scan func(gjson.Result) string
	scan = func(tools gjson.Result) string {
		for _, tool := range tools.Array() {
			kind := tool.Get("type").String()
			if kind == "namespace" {
				if reason := scan(tool.Get("tools")); reason != "" {
					return reason
				}
			}
			switch kind {
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26", "tool_search", "image_generation", "file_search", "code_interpreter", "computer", "computer_use_preview", "mcp":
				return kind
			}
		}
		return ""
	}
	if reason := scan(gjson.GetBytes(raw, "tools")); reason != "" {
		return reason
	}
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			if reason := scan(item.Get("tools")); reason != "" {
				return reason
			}
		}
	}
	return ""
}

package basispoints

import "github.com/tidwall/gjson"

func nativeToolKind(kind string) bool {
	switch kind {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26", "tool_search", "image_generation", "file_search", "code_interpreter", "computer", "computer_use_preview", "mcp":
		return true
	}
	return false
}

func clientToolSearch(tool gjson.Result) bool {
	return tool.Get("type").String() == "tool_search" && tool.Get("execution").String() == "client"
}

// Include discovered tools without treating their declaration as an invocation.
func eachDeclaredTool(raw []byte, visit func(gjson.Result)) {
	var scan func(gjson.Result)
	scan = func(tools gjson.Result) {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "namespace" {
				scan(tool.Get("tools"))
			} else {
				visit(tool)
			}
		}
	}
	scan(gjson.GetBytes(raw, "tools"))
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		kind := item.Get("type").String()
		if kind == "additional_tools" || kind == "tool_search_output" && item.Get("execution").String() == "client" {
			scan(item.Get("tools"))
		}
	}
}

// NativeToolReason makes only decisions known before contacting Basispoints.
// Optional capabilities stay available through the bridge's native-route marker.
func NativeToolReason(raw []byte) string {
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		switch kind := item.Get("type").String(); kind {
		case "web_search_call", "file_search_call", "image_generation_call", "code_interpreter_call", "computer_call", "mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response":
			return kind
		case "tool_search_call", "tool_search_output":
			if item.Get("execution").String() != "client" {
				return kind
			}
		}
	}
	choice := gjson.GetBytes(raw, "tool_choice")
	if choice.String() == "none" {
		return ""
	}
	firstNative, hasClient, clientSearch := "", false, false
	eachDeclaredTool(raw, func(tool gjson.Result) {
		kind := tool.Get("type").String()
		if clientToolSearch(tool) {
			clientSearch, hasClient = true, true
		} else if kind == "function" || kind == "custom" {
			hasClient = true
		} else if nativeToolKind(kind) && firstNative == "" {
			firstNative = kind
		}
	})
	if choice.IsObject() {
		kind := choice.Get("type").String()
		if kind == "tool_search" && clientSearch {
			return ""
		}
		if nativeToolKind(kind) {
			return kind
		}
		// Keep complex native selection contracts intact rather than narrowing them.
		if kind == "allowed_tools" {
			return "tool_choice"
		}
		if kind != "function" && kind != "custom" || choice.Get("name").String() == "" {
			return "tool_choice"
		}
	}
	if choice.String() == "required" && !hasClient {
		return firstNative
	}
	return ""
}

// OptionalNativeTools requires delayed publication so a selected hosted capability
// can switch channels without exposing a speculative answer or executing tools twice.
func OptionalNativeTools(raw []byte) bool {
	if NativeToolReason(raw) != "" {
		return false
	}
	choice := gjson.GetBytes(raw, "tool_choice")
	if choice.IsObject() || choice.String() == "none" {
		return false
	}
	found := false
	eachDeclaredTool(raw, func(tool gjson.Result) {
		if nativeToolKind(tool.Get("type").String()) && !clientToolSearch(tool) {
			found = true
		}
	})
	return found
}

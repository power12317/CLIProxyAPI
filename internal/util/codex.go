package util

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// CodexResponsesLiteToolMode describes whether the request's declared tools
// satisfy the Responses Lite tool contract.
type CodexResponsesLiteToolMode uint8

const (
	CodexResponsesLiteToolsUnknown CodexResponsesLiteToolMode = iota
	CodexResponsesLiteToolsCompatible
	CodexResponsesLiteToolsIncompatible
)

// IsCodexResponsesLiteRequest recognizes the native header and its websocket metadata mirror.
func IsCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	return strings.EqualFold(CodexResponsesLiteHeaderValue(body, headers), "true")
}

// CodexResponsesLiteHeaderValue resolves explicit headers before their body
// mirror. A false value is an explicit choice, not a missing capability hint.
func CodexResponsesLiteHeaderValue(body []byte, headers http.Header) string {
	if value := strings.TrimSpace(headers.Get("X-OpenAI-Internal-Codex-Responses-Lite")); value != "" {
		return value
	}
	value := gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite")
	if value.Type == gjson.True || value.Type == gjson.False {
		return value.String()
	}
	if value.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(value.String())) {
		case "true":
			return "true"
		case "false":
			return "false"
		}
	}
	if value.Exists() {
		return ""
	}
	// Native HTTP requests have no WS mirror. The CLI emits all_turns only
	// for Lite and disables parallel calls in the same request. Recover that
	// explicit wire contract without consulting a model catalog or version.
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Exists() &&
		gjson.GetBytes(body, "reasoning.context").String() == "all_turns" &&
		gjson.GetBytes(body, "parallel_tool_calls").Type == gjson.False {
		return "true"
	}
	return ""
}

// ClassifyCodexResponsesLiteTools checks top-level and additional_tools tool
// declarations without using the Lite header as evidence. An absent tool list
// is unknown; an explicit empty tool list is compatible.
func ClassifyCodexResponsesLiteTools(body []byte) CodexResponsesLiteToolMode {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return CodexResponsesLiteToolsIncompatible
	}

	hasTools := false
	compatible := true
	checkTools := func(tools gjson.Result) {
		if !tools.Exists() {
			return
		}
		hasTools = true
		if !tools.IsArray() {
			compatible = false
			return
		}
		for _, tool := range tools.Array() {
			if !codexResponsesLiteToolCompatible(tool) {
				compatible = false
			}
		}
	}

	checkTools(gjson.GetBytes(body, "tools"))
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			checkTools(item.Get("tools"))
		}
	}
	if !hasTools {
		return CodexResponsesLiteToolsUnknown
	}
	if !compatible {
		return CodexResponsesLiteToolsIncompatible
	}
	return CodexResponsesLiteToolsCompatible
}

func codexResponsesLiteToolCompatible(tool gjson.Result) bool {
	if !tool.IsObject() {
		return false
	}
	switch tool.Get("type").String() {
	case "function", "custom":
		return true
	case "tool_search":
		return tool.Get("execution").String() == "client"
	case "namespace":
		nested := tool.Get("tools")
		if !nested.IsArray() || len(nested.Array()) == 0 {
			return false
		}
		for _, child := range nested.Array() {
			typ := child.Get("type").String()
			if typ != "function" && typ != "custom" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

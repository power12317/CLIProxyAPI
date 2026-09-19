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
	if strings.EqualFold(strings.TrimSpace(headers.Get("X-OpenAI-Internal-Codex-Responses-Lite")), "true") {
		return true
	}
	value := gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite")
	return value.Type == gjson.True || value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true")
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

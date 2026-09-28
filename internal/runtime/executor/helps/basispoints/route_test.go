package basispoints

import (
	"encoding/json"
	"testing"
)

func TestNativeRoutingUsesSelectionInsteadOfDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   any
		reason   string
		optional bool
	}{
		{"auto", "auto", "", true},
		{"default", nil, "", true},
		{"none", "none", "", false},
		{"required with client tools", "required", "", true},
		{"forced search", object{"type": "web_search"}, "web_search", false},
		{"forced client discovery", object{"type": "tool_search"}, "", false},
		{"forced function", object{"type": "function", "name": "exec_command"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := object{"tool_choice": tc.choice, "tools": []any{
				object{"type": "function", "name": "exec_command"},
				object{"type": "tool_search", "execution": "client"},
				object{"type": "web_search", "external_web_access": true},
			}}
			raw, _ := json.Marshal(body)
			if got := NativeToolReason(raw); got != tc.reason {
				t.Fatalf("route=%q want=%q", got, tc.reason)
			}
			if got := OptionalNativeTools(raw); got != tc.optional {
				t.Fatalf("optional=%t want=%t", got, tc.optional)
			}
		})
	}
	for _, kind := range []string{"web_search", "image_generation", "tool_search", "mcp"} {
		body, _ := json.Marshal(object{"tool_choice": "required", "tools": []any{object{"type": kind}}})
		if got := NativeToolReason(body); got != kind {
			t.Fatalf("required native tool lost: %q", got)
		}
	}
}

func TestNativeRoutingPreservesHostedHistory(t *testing.T) {
	for _, kind := range []string{"web_search_call", "image_generation_call", "mcp_call", "tool_search_call"} {
		completed, _ := json.Marshal(object{"input": []any{object{"type": kind, "execution": "server", "status": "completed"}}})
		if NativeToolReason(completed) != "" {
			t.Fatalf("completed native history %s was incorrectly routed", kind)
		}
		active, _ := json.Marshal(object{"input": []any{object{"type": kind, "execution": "server", "status": "in_progress"}}})
		if NativeToolReason(active) != kind {
			t.Fatalf("active native history %s was not routed", kind)
		}
	}
	body, _ := json.Marshal(object{"input": []any{object{"type": "tool_search_call", "execution": "client"}, object{"type": "tool_search_output", "execution": "client", "tools": []any{object{"type": "function", "name": "local"}}}}})
	if NativeToolReason(body) != "" || OptionalNativeTools(body) {
		t.Fatal("client discovery history requires native Codex")
	}
}

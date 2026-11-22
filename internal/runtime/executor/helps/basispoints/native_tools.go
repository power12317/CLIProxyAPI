package basispoints

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MoeForge/moego/json"
)

const nativeRoutePrefix = "cpa.native/"

// NativeToolRequest is an internal routing decision, not an upstream rejection.
// The original client request is forwarded intact before any client tool executes.
type NativeToolRequest struct{ Kind string }

func (e *NativeToolRequest) Error() string { return "native Codex capability requested: " + e.Kind }

func RequestedNativeTool(err error) string {
	var needed *NativeToolRequest
	if errors.As(err, &needed) {
		return needed.Kind
	}
	return ""
}

func (b *Bridge) nativeToolsAvailable() bool {
	return len(b.nativeTools) > 0 && (b.choice == "" || b.choice == "auto" || b.choice == "required")
}

func (b *Bridge) nativeToolInstructions() string {
	if !b.nativeToolsAvailable() {
		return ""
	}
	var text strings.Builder
	text.WriteString("\nThe following native capabilities remain available. Mere availability does not require using them. Use client tools for local work. If a native capability is actually needed, request a channel switch with one run_officejs call: set summary to its exact marker below, code to an empty string, extended_summary to a short explanation, destructive=false and omit optional references. The proxy will forward the original request and complete tool declarations to native Codex; this marker does not execute a client tool. Do not combine a switch request with client tool calls or claim that the capability already ran.\n")
	kinds := make([]string, 0, len(b.nativeTools))
	for kind := range b.nativeTools {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		var descriptions []object
		for _, declaration := range b.nativeTools[kind] {
			view := object{"type": kind}
			// Authentication belongs to the unchanged native request, not a model prompt.
			for _, key := range []string{"description", "server_label", "server_description", "allowed_tools", "require_approval", "external_web_access", "search_context_size", "filters"} {
				if value, exists := declaration[key]; exists {
					view[key] = value
				}
			}
			descriptions = append(descriptions, view)
		}
		declarations, _ := json.Marshal(descriptions)
		fmt.Fprintf(&text, "Native capability %s; marker %s%s; capability descriptions: %s\n", kind, nativeRoutePrefix, kind, declarations)
	}
	return text.String()
}

func (b *Bridge) nativeSelection(args object) error {
	summary := stringValue(args["summary"])
	if !strings.HasPrefix(summary, nativeRoutePrefix) {
		return nil
	}
	kind := strings.TrimPrefix(summary, nativeRoutePrefix)
	if !b.nativeToolsAvailable() || len(b.nativeTools[kind]) == 0 {
		return failure(502, "unexpected_tool", "native capability is not allowed by the current tool catalog and selection")
	}
	if _, ok := args["code"].(string); !ok {
		return failure(502, "invalid_tool_envelope", "native capability marker requires string code")
	}
	if _, ok := args["extended_summary"].(string); !ok {
		return failure(502, "invalid_tool_envelope", "native capability marker requires string extended_summary")
	}
	return &NativeToolRequest{Kind: kind}
}

// Inspect the complete batch before conversion can commit replay entries.
func (b *Bridge) nativeIntent(items []any) error {
	for _, value := range items {
		item, ok := value.(object)
		if !ok || !isTool(item) {
			continue
		}
		name := stringValue(item["name"])
		if !nativeName(name) {
			if item["type"] != "function_call" {
				continue
			}
			if stringValue(item["call_id"]) == "" {
				return failure(502, "invalid_tool_envelope", "native tool call is missing call_id")
			}
			if namespace := stringValue(item["namespace"]); namespace != "" {
				name = namespace + "." + name
			}
			spec, declared := b.lookupTool(name)
			if !declared || b.choice == "none" || b.choice != "" && b.choice != "auto" && b.choice != "required" && b.choice != toolKey(spec) {
				return &NativeToolRequest{Kind: "unmapped_tool/" + name}
			}
			// Direct native calls must fit a real client function contract.
			encrypted := item["encrypted_function_args"]
			if spec.Type == "function" && (encrypted == nil || digest(encrypted) == digest([]string{})) {
				arguments, err := toolObject(item["arguments"], "native_arguments")
				if err == nil && standardPlanTool(spec) {
					err = b.validatePlanTool(spec)
					if err == nil {
						_, err = translatePlanArguments(arguments, spec.Parameters)
					}
				} else if err == nil {
					err = validateFunctionArguments(spec, arguments)
				}
				if err != nil {
					return &NativeToolRequest{Kind: "incompatible_tool/" + name}
				}
			}
			continue
		}
		args, err := toolObject(item["arguments"], "outer_arguments")
		if err != nil {
			continue
		}
		if err = b.nativeSelection(args); err != nil {
			if RequestedNativeTool(err) != "" && stringValue(item["call_id"]) == "" {
				return failure(502, "invalid_tool_envelope", "native capability call is missing call_id")
			}
			return err
		}
	}
	return nil
}

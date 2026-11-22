package basispoints

import (
	"strings"

	"github.com/MoeForge/moego/json"
)

const rawFunctionPrefix = "cpa.function_"

// Only explicitly declared string source parameters use the raw transport.
func rawFunctionField(spec tool) string {
	if spec.Type != "function" {
		return ""
	}
	schema, _ := spec.Parameters.(object)
	if schema["type"] != "object" {
		return ""
	}
	properties, _ := schema["properties"].(object)
	if code, _ := properties["code"].(object); code["type"] == "string" {
		return "code"
	}
	key := toolKey(spec)
	if key == "exec_command" || strings.HasSuffix(key, ".exec_command") {
		if cmd, _ := properties["cmd"].(object); cmd["type"] == "string" {
			return "cmd"
		}
	}
	return ""
}

// Source stays byte-for-byte text. Only the separate metadata is decoded as JSON;
// the existing response converter serializes the combined client arguments.
func (b *Bridge) rawFunctionEnvelope(outer object) (object, bool, error) {
	summary := stringValue(outer["summary"])
	for _, field := range []string{"code", "cmd"} {
		prefix := rawFunctionPrefix + field + "/"
		if !strings.HasPrefix(summary, prefix) {
			continue
		}
		name, extra, hasExtra := strings.Cut(strings.TrimPrefix(summary, prefix), " ")
		if hasExtra && strings.TrimSpace(extra) == "" {
			return nil, true, failure(502, "invalid_tool_envelope", "raw function metadata marker is empty")
		}
		spec, exists := b.tools[name]
		if !exists {
			return nil, true, failure(502, "unexpected_tool", "raw function target is absent from the client catalog")
		}
		if rawFunctionField(spec) != field {
			return nil, true, failure(502, "invalid_tool_arguments", "raw function transport requires the declared string source parameter")
		}
		source, sourceOK := outer["code"].(string)
		metadata, metadataOK := outer["extended_summary"].(string)
		if !sourceOK || !metadataOK {
			return nil, true, failure(502, "invalid_tool_arguments", "raw function transport requires string code and extended_summary")
		}
		if len(source) > maxItemBytes || len(metadata) > maxItemBytes-len(source) {
			return nil, true, failure(502, "tool_item_too_large", "raw function arguments exceed the relay limit")
		}
		args := object{}
		if strings.TrimSpace(extra) != "" {
			var err error
			args, err = decode([]byte(extra))
			if err != nil {
				return nil, true, failure(502, "invalid_tool_envelope", "raw function summary metadata must contain one JSON object")
			}
		}
		// Accept old explicit JSON metadata, but never interpret prose as arguments.
		legacy := strings.TrimSpace(metadata)
		if strings.HasPrefix(legacy, "{") {
			previous, err := decode([]byte(legacy))
			if err != nil {
				return nil, true, failure(502, "invalid_tool_envelope", "legacy function metadata must contain one JSON object")
			}
			for key, value := range previous {
				if existing, present := args[key]; present && digest(existing) != digest(value) {
					return nil, true, failure(502, "invalid_tool_envelope", "conflicting function metadata")
				}
				args[key] = value
			}
		} else if _, err := decodeToolJSON(legacy); err == nil {
			return nil, true, failure(502, "invalid_tool_envelope", "legacy function metadata must be an object")
		}
		if duplicate, exists := args[field]; exists && duplicate != source {
			return nil, true, failure(502, "invalid_tool_envelope", "conflicting raw function source fields")
		}
		args[field] = source
		if err := validateFunctionArguments(spec, args); err != nil {
			return nil, true, err
		}
		encoded, err := json.Marshal(args)
		if err != nil || len(encoded) > maxItemBytes {
			return nil, true, failure(502, "tool_item_too_large", "raw function arguments exceed the relay limit")
		}
		return object{"name": name, "arguments": args}, true, nil
	}
	return nil, false, nil
}

func (b *Bridge) rawFunctionHistory(name string, call, args object) (object, bool, error) {
	spec, exists := b.tools[name]
	if !exists {
		return nil, false, nil
	}
	field := rawFunctionField(spec)
	source, ok := args[field].(string)
	if field == "" || !ok {
		return nil, false, nil
	}
	// Encrypted direct-call history retains its existing envelope semantics.
	if encrypted := call["encrypted_function_args"]; encrypted != nil && digest(encrypted) != digest([]string{}) {
		return nil, false, nil
	}
	metadata := make(object, len(args))
	for key, value := range args {
		if key != field {
			metadata[key] = value
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, true, failure(400, "invalid_tool_history", "function metadata cannot be serialized")
	}
	summary := rawFunctionPrefix + field + "/" + name
	if len(metadata) > 0 {
		summary += " " + string(encoded)
	}
	return object{"summary": summary, "code": source,
		"extended_summary": "Replay the declared client tool", "destructive": false}, true, nil
}

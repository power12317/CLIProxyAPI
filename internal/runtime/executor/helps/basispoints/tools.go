package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
)

const rawCustomPrefix = "cpa.custom/"

func toolKey(spec tool) string {
	if spec.Namespace != "" {
		return spec.Namespace + "." + spec.Name
	}
	return spec.Name
}

func toolInstructions(catalog []tool) string {
	var out strings.Builder
	out.WriteString(" The client's shell, filesystem, planning and other declared tools are available. Use an appropriate tool when needed instead of only announcing an action or claiming it is unavailable. Invoke one client tool through each run_officejs call; the proxy intercepts this transport and does not run Office code. For FUNCTION tools, code contains JSON text {\"name\":\"exact.catalog.name\",\"arguments\":{...}}. For CUSTOM tools, set summary to cpa.custom/exact.catalog.name and put the exact raw input directly in code, following its declared format. Do not wrap raw custom input in another JSON object or Markdown fences. Outer arguments also include extended_summary, destructive=false and references=[]. Use separate calls for multiple tools. Follow the complete tool descriptions and schemas below, including planning tools. After receiving results continue the task and do not repeat completed calls. Client tool catalog:\n")
	for _, spec := range catalog {
		fmt.Fprintf(&out, "\nTool %q (%s): %s\n", toolKey(spec), spec.Type, spec.Description)
		for _, field := range []struct {
			label string
			value any
		}{{"Complete argument schema", spec.Parameters}, {"Exact custom input format", spec.Format}, {"Strict schema", spec.Strict}} {
			if field.value != nil {
				raw, _ := json.Marshal(field.value)
				fmt.Fprintf(&out, "%s: %s\n", field.label, raw)
			}
		}
		if spec.Type == "custom" {
			fmt.Fprintf(&out, "Raw transport summary: %s%s\n", rawCustomPrefix, toolKey(spec))
			if isClientExecTool(spec) {
				out.WriteString("This custom tool runs JavaScript that calls client tools. For a shell command, use raw code such as: const result = await tools.exec_command({\"cmd\":\"pwd\"}); text(result); Put that JavaScript directly in code with the raw transport summary above. The cmd/workdir object belongs to tools.exec_command; it is not the input of this custom tool.\n")
			}
		}
	}
	out.WriteString("\nUse the exact catalog tool name. The CUSTOM summary marker is mandatory for raw input. Do not nest run_officejs wrappers. For FUNCTION tools, serialize code as one JSON object and escape quotes, backslashes, newlines, carriage returns and tabs inside JSON strings.")
	return out.String()
}

func envelopeField(envelope object, primary, alias string) (any, error) {
	first, exists := envelope[primary]
	second, hasAlias := envelope[alias]
	if exists && hasAlias && digest(first) != digest(second) {
		return nil, failure(502, "invalid_tool_envelope", "conflicting tool envelope fields: "+primary+"/"+alias)
	}
	if exists {
		return first, nil
	}
	return second, nil
}

func (b *Bridge) transportEnvelope(arguments object) (object, error) {
	for depth := 0; depth < 3; depth++ {
		// Every nested wrapper can carry a raw custom marker. Its input must
		// bypass JSON recovery so code, patches and grammar input stay exact.
		for _, prefix := range []string{rawCustomPrefix, "codex2api.custom/"} {
			if summary := stringValue(arguments["summary"]); strings.HasPrefix(summary, prefix) {
				input, ok := arguments["code"].(string)
				if !ok {
					return nil, failure(502, "invalid_tool_arguments", "raw custom input must be a string")
				}
				return object{"name": strings.TrimPrefix(summary, prefix), "input": input}, nil
			}
		}
		envelope, err := toolObject(arguments["code"], "transport_code")
		if err != nil {
			if recovered, ok := b.recoverCustomExecEnvelope(arguments["code"]); ok {
				envelope = recovered
			} else if recovered, ok := b.recoverToolEnvelope(arguments["code"]); ok {
				envelope = recovered
			} else {
				return nil, err
			}
		}
		name, err := envelopeField(envelope, "name", "tool")
		if err != nil || !nativeName(stringValue(name)) {
			return envelope, err
		}
		args, err := envelopeField(envelope, "arguments", "args")
		if err != nil {
			return nil, err
		}
		nested, err := toolObject(args, "nested_arguments")
		if err != nil {
			return nil, err
		}
		arguments = nested
	}
	return nil, failure(502, "invalid_tool_envelope", "tool transport nesting exceeds two wrappers")
}

func (b *Bridge) convertTool(native object) (object, error) {
	name := stringValue(native["name"])
	wrapped := nativeName(name)
	var envelope object
	var err error
	if wrapped {
		arguments, errParse := toolObject(native["arguments"], "outer_arguments")
		if errParse != nil {
			return nil, errParse
		}
		envelope, err = b.transportEnvelope(arguments)
		if err != nil {
			return nil, err
		}
	} else {
		if namespace := stringValue(native["namespace"]); namespace != "" {
			name = namespace + "." + name
		}
		envelope = object{"name": name, "arguments": native["arguments"]}
		if native["type"] == "custom_tool_call" {
			envelope = object{"name": name, "input": native["input"]}
		}
	}
	nameValue, err := envelopeField(envelope, "name", "tool")
	if err != nil {
		return nil, err
	}
	key := stringValue(nameValue)
	spec, ok := b.lookupTool(key)
	if !ok || b.choice == "none" || (b.choice != "" && b.choice != "auto" && b.choice != "required" && b.choice != toolKey(spec)) {
		return nil, failure(502, "unexpected_tool", "Basispoints returned a tool outside the selected client catalog")
	}
	callID := stringValue(native["call_id"])
	if callID == "" {
		return nil, failure(502, "invalid_tool_envelope", "tool call is missing call_id")
	}
	itemID := stringValue(native["id"])
	if itemID == "" {
		itemID = functionItemID(callID)
	}
	result := object{"id": itemID, "call_id": callID, "name": spec.Name, "status": "completed"}
	if spec.Namespace != "" {
		result["namespace"] = spec.Namespace
	}
	if spec.Type == "custom" {
		input, errInput := customToolInput(spec, envelope, !wrapped && native["type"] == "function_call")
		if errInput != nil {
			return nil, errInput
		}
		result["type"], result["input"] = "custom_tool_call", input
	} else {
		args, errArgs := envelopeField(envelope, "arguments", "args")
		if errArgs != nil {
			return nil, errArgs
		}
		arguments, errParse := toolObject(args, "function_arguments")
		if errParse != nil {
			return nil, errParse
		}
		preserveEncryption := !wrapped && native["type"] == "function_call"
		encrypted := native["encrypted_function_args"]
		hasEncryptedArguments := preserveEncryption && encrypted != nil && digest(encrypted) != digest([]string{})
		if !wrapped && !hasEncryptedArguments && (name == "update_plan" || name == "functions.update_plan") {
			arguments = translatePlanArguments(arguments, spec.Parameters)
		}
		encoded, _ := json.Marshal(arguments)
		result["type"], result["arguments"] = "function_call", string(encoded)
		// Missing and empty encryption metadata have different client semantics.
		// Relay arguments are plaintext; wrapper metadata describes the wrapper.
		result["encrypted_function_args"] = []string{}
		if preserveEncryption && encrypted != nil {
			result["encrypted_function_args"] = encrypted
		}
	}
	replay := native
	if !wrapped && name != "update_plan" && name != "functions.update_plan" {
		replay, err = rebuildToolCall(result)
		if err != nil {
			return nil, err
		}
	}
	if err = b.cache.put(b.scope+"/"+callID, replay); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *Bridge) lookupTool(key string) (tool, bool) {
	spec, ok := b.tools[key]
	if !ok && strings.HasPrefix(key, "functions.") {
		spec, ok = b.tools[strings.TrimPrefix(key, "functions.")]
	}
	if !ok && key == "update_plan" {
		spec, ok = b.tools["functions.update_plan"]
	}
	return spec, ok
}

func rebuildToolCall(call object) (object, error) {
	id, name := stringValue(call["call_id"]), stringValue(call["name"])
	if id == "" || name == "" {
		return nil, failure(400, "tool_replay_missing", "tool history requires a complete call_id and name")
	}
	if namespace := stringValue(call["namespace"]); namespace != "" {
		name = namespace + "." + name
	}
	if nativeName(name) && call["type"] == "function_call" {
		return clone(call), nil
	}
	envelope := object{"name": name}
	if call["type"] == "custom_tool_call" {
		input, ok := call["input"].(string)
		if !ok {
			return nil, failure(400, "invalid_tool_history", "custom history requires exact input text")
		}
		envelope["input"] = input
	} else {
		args, err := toolObject(call["arguments"])
		if err != nil {
			return nil, failure(400, "invalid_tool_history", "function history requires complete JSON arguments")
		}
		envelope["arguments"] = args
	}
	code, _ := json.Marshal(envelope)
	arguments, _ := json.Marshal(object{"summary": "Client tool " + name, "extended_summary": "Replay client tool result", "code": string(code), "references": []any{}, "destructive": false})
	return object{"type": "function_call", "id": functionItemID(id), "call_id": id, "name": "run_officejs", "arguments": string(arguments), "status": "completed"}, nil
}

func translatePlanArguments(arguments object, parameters any) object {
	schema, _ := parameters.(object)
	properties, _ := schema["properties"].(object)
	if properties["plan"] == nil {
		return arguments
	}
	steps, ok := arguments["plan"].([]any)
	if !ok {
		return arguments
	}
	result := clone(arguments)
	if _, exists := result["explanation"]; !exists && result["summary"] != nil {
		result["explanation"] = result["summary"]
		delete(result, "summary")
	}
	plan := make([]any, 0, len(steps))
	for _, value := range steps {
		step, ok := value.(object)
		if !ok {
			return arguments
		}
		converted := clone(step)
		if _, exists := converted["step"]; !exists {
			for _, alias := range []string{"description", "title"} {
				if text, ok := converted[alias].(string); ok {
					converted["step"] = text
					delete(converted, alias)
					break
				}
			}
		}
		switch strings.ToLower(stringValue(converted["status"])) {
		case "todo", "not_started", "planned", "queued", "blocked":
			converted["status"] = "pending"
		case "active", "started", "doing", "current":
			converted["status"] = "in_progress"
		case "done", "complete", "finished":
			converted["status"] = "completed"
		}
		plan = append(plan, converted)
	}
	result["plan"] = plan
	return result
}

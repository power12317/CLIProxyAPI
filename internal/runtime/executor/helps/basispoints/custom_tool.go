package basispoints

import (
	"encoding/json"
	"regexp"
	"strings"
)

func isClientExecTool(spec tool) bool {
	key := toolKey(spec)
	return spec.Type == "custom" && (key == "functions.exec" || (key == "exec" && strings.Contains(strings.ToLower(spec.Description), "javascript")))
}

var clientProgramStart = regexp.MustCompile(`(?s)^(?:\s|//[^\n]*\n|/\*.*?\*/)*(?:(?:const|let|var|await|return|for|if|try|switch|while|do|throw|async|function)\b|(?:tools\s*[.\[]|ALL_TOOLS\b|(?:text|image|audio|generatedImage|store|load|notify|exit|yield_control|setTimeout|clearTimeout)\s*\())`)
var clientRuntimeUse = regexp.MustCompile(`\b(?:tools\s*[.\[]|ALL_TOOLS\b|(?:text|image|audio|generatedImage|store|load|notify|exit|yield_control|setTimeout|clearTimeout)\s*\()`)

// Recover unmarked programs only for the declared client JavaScript runtime.
// Keep the whole program: JSON objects inside it can be data, not tool calls.
func (b *Bridge) recoverCustomExecEnvelope(value any) (object, bool) {
	program, ok := value.(string)
	if !ok || !clientProgramStart.MatchString(program) || !clientRuntimeUse.MatchString(program) {
		return nil, false
	}
	var name string
	for _, spec := range b.tools {
		if isClientExecTool(spec) {
			if name != "" {
				return nil, false
			}
			name = toolKey(spec)
		}
	}
	if name == "" {
		return nil, false
	}
	return object{"name": name, "input": program}, true
}

// customToolInput keeps raw text intact and accepts the existing envelope
// aliases. Only the named JavaScript exec tool adapts a shell argument object.
func customToolInput(spec tool, envelope object, encodedFunctionArguments bool) (string, error) {
	var value any
	var field string
	for _, key := range []string{"input", "args", "arguments"} {
		candidate, exists := envelope[key]
		if !exists {
			continue
		}
		if field != "" && digest(value) != digest(candidate) {
			return "", failure(502, "invalid_tool_envelope", "conflicting custom input fields: "+field+"/"+key)
		}
		value, field = candidate, key
	}
	if encodedFunctionArguments && field == "arguments" && isClientExecTool(spec) {
		if text, ok := value.(string); ok {
			if args, err := decode([]byte(text)); err == nil {
				if _, isCommand := args["cmd"].(string); isCommand {
					value = args
				}
			}
		}
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	if args, ok := value.(object); ok && isClientExecTool(spec) {
		if _, isCommand := args["cmd"].(string); isCommand {
			encoded, err := json.Marshal(args)
			if err != nil {
				return "", failure(502, "invalid_tool_arguments", "exec command arguments cannot be serialized")
			}
			// Serialize all parameters as data. CPA never executes this program;
			// the client invokes its own exec_command with the original values.
			return "const result = await tools.exec_command(" + string(encoded) + "); text(result);", nil
		}
	}
	return "", failure(502, "invalid_tool_arguments", "custom tool input must be a string")
}

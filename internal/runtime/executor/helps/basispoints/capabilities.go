package basispoints

import "sort"

// validateFunctionArguments uses the declared client contract without inferring values.
func validateFunctionArguments(spec tool, arguments object) error {
	if spec.Parameters == nil {
		return nil
	}
	schema, ok := spec.Parameters.(object)
	if !ok {
		return failure(502, "invalid_tool_arguments", "client function schema is not an object")
	}
	compiled, err := compileLocalSchema(schema)
	if err != nil || compiled.Validate(arguments) != nil {
		return failure(502, "invalid_tool_arguments", "function arguments do not satisfy the declared client schema")
	}
	return nil
}

// responseToolCatalog describes actual client handlers and declared hosted capabilities.
func responseToolCatalog(catalog []tool, hosted map[string][]object) []any {
	result := make([]any, 0, len(catalog)+len(hosted))
	namespaces := make(map[string]object)
	for _, spec := range catalog {
		declaration := object{"type": spec.Type, "name": spec.Name}
		if spec.Type == "tool_search" {
			delete(declaration, "name")
			declaration["execution"] = "client"
		}
		for key, value := range (object{"description": spec.Description, "parameters": spec.Parameters, "format": spec.Format, "strict": spec.Strict}) {
			if value != nil && value != "" {
				declaration[key] = value
			}
		}
		if spec.Namespace == "" {
			result = append(result, declaration)
			continue
		}
		namespace := namespaces[spec.Namespace]
		if namespace == nil {
			namespace = object{"type": "namespace", "name": spec.Namespace, "tools": []any{}}
			namespaces[spec.Namespace] = namespace
			result = append(result, namespace)
		}
		namespace["tools"] = append(namespace["tools"].([]any), declaration)
	}
	kinds := make([]string, 0, len(hosted))
	for kind := range hosted {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for _, declaration := range hosted[kind] {
			result = append(result, clone(declaration))
		}
	}
	return result
}

func (b *Bridge) decorateToolCatalog(response object) {
	if _, present := response["tools"]; present {
		response["tools"] = b.clientCatalog
	}
	if _, present := response["tool_choice"]; present && b.clientChoice != nil {
		response["tool_choice"] = b.clientChoice
	}
}

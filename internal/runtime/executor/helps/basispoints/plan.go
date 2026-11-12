package basispoints

import "strings"

func standardPlanTool(spec tool) bool {
	key := toolKey(spec)
	return key == "update_plan" || key == "functions.update_plan"
}

func (b *Bridge) validatePlanTool(selected tool) error {
	if selected.Type != "function" {
		return failure(502, "invalid_plan", "native plan requires a declared client function")
	}
	matches := 0
	for _, candidate := range b.tools {
		if standardPlanTool(candidate) {
			matches++
		}
	}
	if matches != 1 {
		return failure(502, "invalid_plan", "native plan requires an unambiguous client declaration")
	}
	return nil
}

func planAlias(value object, keys ...string) (string, bool, error) {
	var result string
	present := false
	for _, key := range keys {
		raw, exists := value[key]
		if !exists {
			continue
		}
		text, ok := raw.(string)
		if !ok || present && result != text {
			return "", false, failure(502, "invalid_plan", "plan text fields are invalid or conflicting")
		}
		result, present = text, true
	}
	return result, present, nil
}

func translatePlanArguments(arguments object, parameters any) (object, error) {
	invalid := func() (object, error) {
		return nil, failure(502, "invalid_plan", "Basispoints plan does not satisfy the declared client contract")
	}
	schema, ok := parameters.(object)
	if !ok {
		return invalid()
	}
	properties, _ := schema["properties"].(object)
	if schema["type"] != "object" || properties["plan"] == nil {
		return invalid()
	}
	steps, ok := arguments["plan"].([]any)
	if !ok {
		return invalid()
	}
	result := clone(arguments)
	explanation, present, err := planAlias(arguments, "explanation", "summary")
	if err != nil {
		return nil, err
	}
	if present {
		result["explanation"] = explanation
		delete(result, "summary")
	}
	plan := make([]any, 0, len(steps))
	for _, value := range steps {
		step, ok := value.(object)
		if !ok {
			return invalid()
		}
		description, present, err := planAlias(step, "step", "description", "title")
		if err != nil {
			return nil, err
		}
		if !present || strings.TrimSpace(description) == "" {
			return invalid()
		}
		converted := clone(step)
		converted["step"] = description
		delete(converted, "description")
		delete(converted, "title")
		status := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(stringValue(step["status"]))))
		switch status {
		case "pending", "todo", "not_started", "planned", "queued", "blocked":
			status = "pending"
		case "in_progress", "active", "started", "doing", "current":
			status = "in_progress"
		case "completed", "done", "complete", "finished":
			status = "completed"
		default:
			return invalid()
		}
		converted["status"] = status
		plan = append(plan, converted)
	}
	result["plan"] = plan
	compiled, err := compileLocalSchema(schema)
	if err != nil || compiled.Validate(result) != nil {
		return invalid()
	}
	return result, nil
}

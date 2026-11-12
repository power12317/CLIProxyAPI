package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func planContract() object {
	return object{"type": "object", "properties": object{
		"plan": object{"type": "array", "minItems": 1, "items": object{"type": "object", "properties": object{
			"step": object{"type": "string", "minLength": 1}, "status": object{"enum": []any{"pending", "in_progress", "completed"}},
		}, "required": []any{"step", "status"}, "additionalProperties": false}},
		"explanation": object{"type": "string"},
	}, "required": []any{"plan"}, "additionalProperties": false}
}

func TestPlanArgumentsValidateDeclaredContract(t *testing.T) {
	for _, tc := range []struct {
		name, arguments string
		valid           bool
	}{
		{"aliases", `{"summary":"update","plan":[{"description":"work","status":"in-progress"}]}`, true},
		{"identical aliases", `{"explanation":"same","summary":"same","plan":[{"step":"work","title":"work","status":"done"}]}`, true},
		{"conflicting step", `{"plan":[{"step":"first","description":"second","status":"done"}]}`, false},
		{"conflicting explanation", `{"explanation":"first","summary":"second","plan":[{"step":"work","status":"done"}]}`, false},
		{"unknown status", `{"plan":[{"step":"work","status":"unknown"}]}`, false},
		{"missing status", `{"plan":[{"step":"work"}]}`, false},
		{"empty step", `{"plan":[{"step":" ","status":"done"}]}`, false},
		{"additional property", `{"plan":[{"step":"work","status":"done","unexpected":true}]}`, false},
		{"wrong plan type", `{"plan":"work"}`, false},
		{"empty plan", `{"plan":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := decode([]byte(tc.arguments))
			before := digest(args)
			result, err := translatePlanArguments(args, planContract())
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected result: %#v %v", result, err)
			}
			if before != digest(args) {
				t.Fatal("source arguments mutated")
			}
		})
	}
}

func TestPlanConversionRejectsAmbiguousAndInvalidCalls(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		source := object{"model": "test-model", "input": "plan", "tools": []any{object{"type": "function", "name": "update_plan", "parameters": planContract()}}}
		raw, _ := json.Marshal(source)
		_, b, err := Prepare(raw, "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := object{"type": "function_call", "name": "update_plan", "call_id": "plan", "arguments": `{"plan":[{"step":"work","status":"invalid"}]}`}
		if wrapped {
			envelope := object{"name": "update_plan", "arguments": object{"plan": []any{object{"step": "work", "status": "invalid"}}}}
			code, _ := json.Marshal(envelope)
			outer, _ := json.Marshal(object{"code": string(code)})
			native["name"], native["arguments"] = "run_officejs", string(outer)
		}
		if _, err = b.convertTool(native); err == nil || !strings.Contains(err.Error(), "invalid_plan") {
			t.Fatal("invalid plan accepted")
		}
		if len(b.cache.items) != 0 {
			t.Fatal("invalid call entered replay cache")
		}
		b.tools["functions.update_plan"] = tool{Name: "update_plan", Namespace: "functions", Type: "function", Parameters: planContract()}
		native["name"], native["arguments"] = "update_plan", `{"plan":[{"step":"work","status":"done"}]}`
		if _, err = b.convertTool(native); err == nil {
			t.Fatal("ambiguous plan declaration accepted")
		}
	}
}

func TestQualifiedLeafPlanNameStillValidated(t *testing.T) {
	source := object{"model": "test-model", "input": "plan", "tools": []any{object{"type": "function", "name": "functions.update_plan", "parameters": planContract()}}}
	raw, _ := json.Marshal(source)
	_, b, err := Prepare(raw, "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.convertTool(object{"type": "function_call", "name": "functions.update_plan", "call_id": "plan", "arguments": `{"plan":[{"step":"work","status":"invalid"}]}`})
	if err == nil {
		t.Fatal("qualified plan name bypassed validation")
	}
}

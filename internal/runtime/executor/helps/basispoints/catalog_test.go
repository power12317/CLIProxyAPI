package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoricalCatalogUpdatesKeepPastRawTransport(t *testing.T) {
	old := object{"type": "function", "name": "run", "parameters": object{"type": "object", "properties": object{"code": object{"type": "string"}}}}
	latest := object{"type": "function", "name": "run", "parameters": object{"type": "object", "properties": object{"other": object{"type": "string"}}}}
	source := object{"model": "test-model", "tools": []any{latest}, "input": []any{
		object{"type": "additional_tools", "tools": []any{old}},
		object{"type": "function_call", "call_id": "old-call", "name": "run", "arguments": `{"code":"exact old source"}`},
		object{"type": "function_call_output", "call_id": "old-call", "output": "past result"},
		object{"type": "additional_tools", "tools": []any{latest}},
		message("user", "continue"),
	}}
	raw, _ := json.Marshal(source)
	for _, scope := range []string{"account-a", "account-b"} {
		wire, b, err := Prepare(raw, scope, "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		spec := b.tools["run"]
		if digest(spec.Parameters) != digest(latest["parameters"]) {
			t.Fatal("latest schema not active")
		}
		body, _ := decode(wire)
		input := body["input"].([]any)
		native := input[1].(object)
		args, err := decode([]byte(stringValue(native["arguments"])))
		if err != nil || args["summary"] != "cpa.function_code/run" || args["code"] != "exact old source" {
			t.Fatalf("past call rebuilt using latest schema: %s", wire)
		}
		if strings.Contains(string(wire), `"type":"additional_tools"`) {
			t.Fatal("catalog history leaked upstream")
		}
	}
}

func TestCatalogConflictsAreScopedToBatches(t *testing.T) {
	first := object{"type": "function", "name": "run", "parameters": object{"type": "object"}}
	second := object{"type": "custom", "name": "run"}
	for _, history := range []bool{false, true} {
		source := object{"model": "test-model", "input": "hello"}
		if history {
			source["input"] = []any{object{"type": "additional_tools", "tools": []any{first, second}}}
		} else {
			source["tools"] = []any{first, second}
		}
		raw, _ := json.Marshal(source)
		if _, _, err := Prepare(raw, "scope", "session", &Cache{}); err == nil {
			t.Fatal("conflicting batch accepted")
		}
	}
	source := object{"model": "test-model", "tools": []any{first}, "input": []any{object{"type": "additional_tools", "tools": []any{second}}}}
	raw, _ := json.Marshal(source)
	_, b, err := Prepare(raw, "scope", "session", &Cache{})
	if err != nil || b.tools["run"].Type != "custom" {
		t.Fatalf("later batch not applied: %v", err)
	}
	// A dotted leaf and a nested namespace must not alias the same callable.
	source["tools"] = []any{object{"type": "function", "name": "functions.run"}}
	source["input"] = []any{object{"type": "additional_tools", "tools": []any{object{"type": "namespace", "name": "functions", "tools": []any{first}}}}}
	raw, _ = json.Marshal(source)
	if _, _, err := Prepare(raw, "scope", "session", &Cache{}); err == nil {
		t.Fatal("ambiguous namespaces accepted")
	}
}

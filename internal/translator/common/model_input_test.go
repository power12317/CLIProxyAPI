package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestPreserveModelInputFieldsConsumesOnlyConvertedStructures(t *testing.T) {
	source := gjson.Parse(`{"messages":[{"role":"user","content":"source"}],"model":"caller-alias","reasoning":{"context":"all_turns","future":{"large":9007199254740993}},"include":[],"text":{"format":{"type":"json_object"}},"optional":null,"future.field":{"a:b":{"value":false}}}`)
	converted := []byte(`{"model":"resolved-model","input":[{"role":"user","content":"converted"}],"reasoning":{"effort":"high"},"include":["default"],"text":{"verbosity":"low"}}`)
	actual := PreserveModelInputFields(converted, source, "messages", "model")
	for path, expected := range map[string]string{
		"model": `"resolved-model"`, "input.0.content": `"converted"`,
		"reasoning.effort": `"high"`, "reasoning.context": `"all_turns"`,
		"reasoning.future.large": "9007199254740993", "include": "[]", "optional": "null",
		"text.verbosity": `"low"`, "text.format.type": `"json_object"`,
		`future\.field.a:b.value`: "false",
	} {
		if got := gjson.GetBytes(actual, path).Raw; got != expected {
			t.Fatalf("%s = %s, want %s; body=%s", path, got, expected, actual)
		}
	}
	if gjson.GetBytes(actual, "messages").Exists() {
		t.Fatalf("consumed Chat messages were restored: %s", actual)
	}
}

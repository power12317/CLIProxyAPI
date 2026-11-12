package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func structuredBridge(t *testing.T, format object) *Bridge {
	t.Helper()
	raw, _ := json.Marshal(object{"model": "test-model", "input": "answer", "text": object{"format": format}})
	wire, b, err := Prepare(raw, "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"text":{"format"`)) {
		t.Fatal("unsupported native output format sent upstream")
	}
	if format["type"] != "text" && !bytes.Contains(wire, []byte("gateway validates")) {
		t.Fatal("missing format instructions")
	}
	return b
}

func structuredMessage(answer string) object {
	return object{"type": "message", "id": "message-id", "role": "assistant", "status": "completed", "content": []any{object{"type": "output_text", "text": answer, "annotations": []any{}}}}
}

func TestStructuredOutputResponseContracts(t *testing.T) {
	schema := object{"type": "json_schema", "name": "answer", "strict": true, "schema": object{
		"type": "object", "properties": object{"answer": object{"$ref": "#/$defs/answer"}},
		"required": []any{"answer"}, "additionalProperties": false,
		"$defs": object{"answer": object{"type": "integer", "minimum": json.Number("9007199254740993")}},
	}}
	for _, tc := range []struct {
		name   string
		format object
		answer string
		valid  bool
	}{
		{"object", object{"type": "json_object"}, `{"ok":true}`, true},
		{"array", object{"type": "json_object"}, `[]`, false},
		{"null", object{"type": "json_object"}, `null`, false},
		{"number", object{"type": "json_object"}, `7`, false},
		{"invalid JSON", object{"type": "json_object"}, `private-invalid-answer`, false},
		{"multiple values", object{"type": "json_object"}, `{} {}`, false},
		{"schema and precision", schema, `{"answer":9007199254740993}`, true},
		{"minimum", schema, `{"answer":9007199254740992}`, false},
		{"required", schema, `{}`, false},
		{"schema scalar", object{"type": "json_schema", "name": "scalar", "schema": object{"type": "string"}}, `"allowed"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := structuredBridge(t, tc.format)
			raw, _ := json.Marshal(object{"status": "completed", "usage": object{"total_tokens": 7}, "output": []any{structuredMessage(tc.answer)}})
			result, err := b.Response(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected result: %s %v", result, err)
			}
			if tc.valid {
				response, _ := decode(result)
				text := response["text"].(object)
				if digest(text["format"]) != digest(tc.format) || response["usage"].(object)["total_tokens"] != json.Number("7") {
					t.Fatal("format or usage lost")
				}
			} else if strings.Contains(err.Error(), tc.answer) {
				t.Fatal("invalid answer exposed in error")
			}
		})
	}
}

func TestStructuredOutputRejectsInvalidSchemaWithoutExternalIO(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, format := range []any{
		nil, "invalid", object{}, object{"type": "json_schema", "name": "bad name", "schema": object{}},
		object{"type": "json_schema", "name": "schema", "strict": nil, "schema": object{}},
		object{"type": "json_schema", "name": "schema", "schema": object{"$ref": server.URL + "/private-schema"}},
		object{"type": "json_schema", "name": "schema", "schema": object{"$ref": "file:///private/schema.json"}},
		object{"type": "json_schema", "name": "schema", "schema": object{"type": 123}},
		object{"type": "json_schema", "name": "schema", "schema": object{"description": strings.Repeat("x", 1<<20)}},
		object{"type": "json_object", "schema": object{}},
	} {
		raw, _ := json.Marshal(object{"model": "test-model", "input": "answer", "text": object{"format": format}})
		_, _, err := Prepare(raw, "account", "session", &Cache{})
		var invalid *Error
		if !errors.As(err, &invalid) || invalid.Status != 400 {
			t.Fatalf("invalid format accepted: %T %v", format, err)
		}
		if strings.Contains(err.Error(), "private-schema") {
			t.Fatal("schema URL exposed in error")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("schema compilation performed external requests")
	}
}

func TestStructuredStreamValidatesTerminalBeforePublishingText(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		valid        bool
	}{
		{"valid", `{"ok":true}`, true}, {"invalid", `not-json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := structuredBridge(t, object{"type": "json_object"})
			var upstream, output bytes.Buffer
			for _, event := range []object{
				{"type": "response.created", "response": object{"output": []any{structuredMessage("unvalidated snapshot")}}},
				{"type": "response.output_item.added", "output_index": 0, "item": structuredMessage("unvalidated added")},
				{"type": "response.output_text.delta", "item_id": "message-id", "delta": "unvalidated delta"},
				{"type": "response.output_item.done", "output_index": 0, "item": structuredMessage("unvalidated done")},
				{"type": "response.completed", "response": object{"status": "completed", "output": []any{structuredMessage(tc.answer)}}},
			} {
				raw, _ := json.Marshal(event)
				upstream.WriteString("data: " + string(raw) + "\n\n")
			}
			err := b.Stream(&upstream, func(raw []byte) error { output.Write(raw); return nil })
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected stream result: %v", err)
			}
			if strings.Contains(output.String(), "unvalidated") || !tc.valid && strings.Contains(output.String(), "not-json") {
				t.Fatal("unvalidated message escaped")
			}
			if tc.valid {
				if strings.Count(output.String(), "event: response.output_text.delta") != 1 || strings.Count(output.String(), "event: response.output_item.done") != 1 {
					t.Fatal("validated message not emitted exactly once")
				}
				if !strings.Contains(output.String(), `\"ok\":true`) {
					t.Fatal("authoritative final answer lost")
				}
			}
		})
	}
}

func TestStructuredOutputToolRefusalAndIncompleteSemantics(t *testing.T) {
	for _, kind := range []string{"tool", "refusal", "incomplete", "failed"} {
		t.Run(kind, func(t *testing.T) {
			b := structuredBridge(t, object{"type": "json_object"})
			b.tools["weather"] = tool{Name: "weather", Type: "function"}
			response := object{"status": "completed", "output": []any{nativeWeather()}}
			eventType := "response.completed"
			switch kind {
			case "refusal":
				response["output"] = []any{object{"type": "message", "id": "refusal", "content": []any{object{"type": "refusal", "refusal": "declined"}}}}
			case "incomplete":
				response["status"] = "incomplete"
				response["output"] = []any{structuredMessage(`{"partial":`)}
				eventType = "response.incomplete"
			case "failed":
				response["status"] = "failed"
				response["error"] = object{"code": "upstream_failure"}
				response["output"] = []any{structuredMessage("private failed output")}
				eventType = "response.failed"
			}
			raw, _ := json.Marshal(object{"type": eventType, "response": response})
			var output bytes.Buffer
			err := b.Stream(strings.NewReader("data: "+string(raw)+"\n\n"), func(raw []byte) error { output.Write(raw); return nil })
			wantError := kind == "incomplete" || kind == "failed"
			if (err != nil) != wantError {
				t.Fatalf("unexpected %s result: %v", kind, err)
			}
			if wantError && output.Len() != 0 {
				t.Fatal("incomplete or failed message published")
			}
			if kind == "tool" && !strings.Contains(output.String(), `"name":"weather"`) {
				t.Fatal("tool turn blocked by JSON contract")
			}
			if kind == "refusal" && !strings.Contains(output.String(), "response.refusal.delta") {
				t.Fatal("refusal suppressed")
			}
			if kind == "failed" && !strings.Contains(err.Error(), "upstream_failure") {
				t.Fatal("upstream error masked by validation")
			}
		})
	}
}

func TestStructuredInstructionsAddedOnceAfterClientInstructions(t *testing.T) {
	raw := []byte(`{"model":"test-model","instructions":"Keep exact user instructions","input":"answer","text":{"format":{"type":"json_object"}}}`)
	wire, _, err := Prepare(raw, "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(wire), "gateway validates") != 1 {
		t.Fatal("structured instruction duplicated")
	}
	body, _ := decode(wire)
	first := body["input"].([]any)[0].(object)["content"].([]any)[0].(object)["text"]
	if first != "Keep exact user instructions" {
		t.Fatal("client instructions rewritten")
	}
}

package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// The wire fields and history shapes follow both reference adapters. Ordinary
// Responses fields are not automatically valid in the Basispoints API schema.
func TestPrepareUsesBasispointsRequestSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-6-astra","stream":true,"store":true,
		"instructions":"Answer briefly.","reasoning_effort":"xhigh",
		"reasoning":{"summary":"auto"},"include":["reasoning.encrypted_content"],
		"text":{"verbosity":"low"},"max_output_tokens":1234,
		"type":"response.create","generate":true,"prompt_cache_retention":"24h",
		"service_tier":"default","context_management":null,
		"prompt_cache_key":"conversation-1","client_metadata":{"session_id":"conversation-1"},
		"metadata":{"attempt":2,"enabled":true,"nested":{"private":"omit"}},
		"input":[
			{"role":"user","content":"Reply OK","internal_chat_message_metadata_passthrough":{"turn_id":"client-only"}},
			{"type":"reasoning","id":"rs_bare","summary":[]},
			{"type":"item_reference","id":"rs_reference"},
			{"type":"reasoning","id":"rs_encrypted","status":"completed","summary":[{"type":"summary_text","text":"private summary"}],"encrypted_content":"opaque-state"}
		]
	}`)
	body, _, err := Prepare(raw, "account/caller", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	var wire object
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"model": true, "model_selection": true, "stream": true, "store": true, "input": true, "reasoning_effort": true, "prompt_cache_key": true, "metadata": true}
	for field := range wire {
		if !allowed[field] {
			t.Errorf("non-Basispoints field reached the wire: %s", field)
		}
	}
	if wire["model"] != "gpt-6-astra" || wire["reasoning_effort"] != "xhigh" || wire["model_selection"] != "explicit" || wire["store"] != false {
		t.Fatalf("model, effort or required wire fields changed: %s", body)
	}
	input := wire["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("want instruction, catalog, user and encrypted reasoning; got %s", body)
	}
	if gjson.GetBytes(body, "input.0.content.0.text").String() != "Answer briefly." ||
		gjson.GetBytes(body, "input.2.internal_chat_message_metadata_passthrough").Exists() {
		t.Fatalf("instructions or message metadata not translated: %s", body)
	}
	wantReasoning := object{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-state"}
	if !reflect.DeepEqual(input[3], wantReasoning) {
		t.Fatalf("reasoning replay must use the stateless wire shape: %#v", input[3])
	}
	metadata := wire["metadata"].(map[string]any)
	if metadata["attempt"] != "2" || metadata["enabled"] != "true" || metadata["nested"] != nil {
		t.Fatalf("metadata must contain scalar strings: %#v", metadata)
	}
}

func TestPrepareOptionalWireFields(t *testing.T) {
	for _, tc := range []struct {
		name, fields, key, want string
	}{
		{"no-policy", ``, "context_management", ""},
		{"null-policy", `,"context_management":null`, "context_management", ""},
		{"empty-policy", `,"context_management":[]`, "context_management", ""},
		{"explicit-policy", `,"context_management":[{"type":"compaction","compact_threshold":475000}]`, "context_management", `[{"compact_threshold":475000,"type":"compaction"}]`},
		{"invalid-policy-for-upstream", `,"context_management":"invalid"`, "context_management", `"invalid"`},
		{"no-tier", ``, "service_tier", ""},
		{"null-tier", `,"service_tier":null`, "service_tier", ""},
		{"auto-tier", `,"service_tier":"auto"`, "service_tier", ""},
		{"default-tier", `,"service_tier":"default"`, "service_tier", ""},
		{"priority-not-downgraded", `,"service_tier":"priority"`, "service_tier", `"priority"`},
		{"fast-alias", `,"service_tier":"fast"`, "service_tier", `"priority"`},
		{"ultrafast-not-downgraded", `,"service_tier":"ultrafast"`, "service_tier", `"ultrafast"`},
		{"ordinary-tier-omitted", `,"service_tier":"standard"`, "service_tier", ""},
		{"unknown-tier-omitted", `,"service_tier":"future"`, "service_tier", ""},
		{"effort-not-downgraded", `,"reasoning_effort":"max"`, "reasoning_effort", `"max"`},
		{"no-default-effort", ``, "reasoning_effort", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := Prepare([]byte(`{"model":"unchanged","input":"hello"`+tc.fields+`}`), "scope", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, tc.key).Raw; got != tc.want {
				t.Fatalf("wire %s = %s, want %s", tc.key, got, tc.want)
			}
		})
	}
}

func TestPrepareMetadataLimitsDoNotSplitUnicodeOrRoundNumbers(t *testing.T) {
	raw, err := json.Marshal(object{
		"model": "unchanged", "input": "hello",
		"metadata": object{strings.Repeat("界", 65): strings.Repeat("界", 513), "large": json.Number("9007199254740993"), "list": []any{1}, "null": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Prepare(raw, "scope", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := gjson.GetBytes(body, "metadata").Map()
	if len(metadata) != 5 || metadata["large"].String() != "9007199254740993" || metadata[strings.Repeat("界", 64)].String() != strings.Repeat("界", 512) {
		t.Fatalf("metadata is not wire-safe: %s", body)
	}
	if !utf8.Valid(body) {
		t.Fatal("metadata truncation produced invalid UTF-8")
	}
}

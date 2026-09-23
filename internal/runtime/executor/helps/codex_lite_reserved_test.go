package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexReservedFixtureProvenance(t *testing.T) {
	raw, err := os.ReadFile("codex_lite_fixtures/0.156.0/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Commit string            `json:"cli_commit"`
		Hashes map[string]string `json:"fixtures_sha256"`
	}
	if err := json.Unmarshal(raw, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Commit != "fe74a774532af67b5a4a3dec03ce9469e17f89af" {
		t.Fatal("unexpected CLI baseline")
	}
	for name, data := range map[string]string{"image_gen.json": codexLiteImageNamespace, "web.json": codexLiteWebNamespace} {
		digest := sha256.Sum256([]byte(data))
		if hex.EncodeToString(digest[:]) != provenance.Hashes[name] {
			t.Fatalf("%s no longer matches the recorded Rust export", name)
		}
	}
	image := gjson.Parse(codexLiteImageNamespace).Get("tools.0")
	for _, path := range []string{"parameters.properties.referenced_image_paths.maxItems", "parameters.properties.num_last_images_to_include.minimum", "parameters.properties.num_last_images_to_include.maximum"} {
		if image.Get(path).Exists() {
			t.Fatalf("non-wire constraint survived: %s", path)
		}
	}
	if len(image.Get("description").String()) < 1500 || image.Get("parameters.properties.referenced_image_paths.items.description").String() == "" {
		t.Fatal("official function or path description lost")
	}
	web := gjson.Parse(codexLiteWebNamespace).Get("tools.0.parameters")
	if web.Get("required").Exists() || len(web.Get("properties").Map()) != 11 {
		t.Fatal("web schema truncated or required search imposed")
	}
	for _, field := range []string{"search_query", "image_query", "open", "click", "find", "screenshot", "finance", "weather", "sports", "time", "response_length"} {
		if !web.Get("properties." + field).Exists() {
			t.Fatalf("official web command missing: %s", field)
		}
	}
}

func TestCodexReservedCanonicalDeclarationsPreserveHistoryIdentity(t *testing.T) {
	body := []byte(`{"previous_response_id":"prior","input":[{"id":"at-client","type":"additional_tools","tools":[]},{"type":"function_call","namespace":"image_gen","name":"imagegen","call_id":"c","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`)
	body, _ = sjson.SetRawBytes(body, "input.0.tools.-1", []byte(codexLiteImageNamespace))
	body, _ = sjson.SetRawBytes(body, "input.0.tools.-1", []byte(codexLiteWebNamespace))
	got := mustNormalizeCodexLiteTools(t, body, true)
	if string(got) != string(body) {
		t.Fatal("verified declaration, item identity or history changed")
	}
	body, _ = sjson.SetRawBytes(body, "input.0.tools.0", []byte(codexLiteImageNamespace154))
	if got := mustNormalizeCodexLiteTools(t, body, true); string(got) != string(body) {
		t.Fatal("verified older client declaration changed")
	}
}

func TestCodexReservedLegacyTemplatesRepairOnlyFreshContext(t *testing.T) {
	for _, spec := range []struct{ legacy, canonical string }{{codexLegacyImageNamespace, codexLiteImageNamespace}, {codexLegacyWebNamespace, codexLiteWebNamespace}} {
		body := []byte(`{"input":[],"tools":[]}`)
		body, _ = sjson.SetRawBytes(body, "tools.-1", []byte(spec.legacy))
		got := mustNormalizeCodexLiteTools(t, body, false)
		if !codexToolJSONEqual(gjson.GetBytes(got, "input.0.tools.0").Raw, spec.canonical) {
			t.Fatal("known fresh legacy template was not replaced completely")
		}
		id := gjson.GetBytes(got, "input.0.id").String()
		if id == "" || id == codexLiteItemID(body, "at", []byte("["+spec.legacy+"]")) {
			t.Fatal("changed declaration retained legacy identity")
		}
		for _, history := range []string{
			`{"previous_response_id":"prior"}`,
			`{"input":[{"id":"at-old","type":"additional_tools","tools":[]}]}`,
			`{"input":[{"type":"function_call_output","call_id":"prior","output":"result"}]}`,
		} {
			old, _ := sjson.SetRaw(history, "tools", gjson.GetBytes(body, "tools").Raw)
			_, err := NormalizeCodexLiteCompatibilityTools([]byte(old), false)
			var conflict *codexReservedToolSchemaError
			if !errors.As(err, &conflict) || !conflict.legacy || conflict.StatusCode() != 400 || !conflict.IsRequestScoped() {
				t.Fatalf("old context accepted or incorrectly classified: %v", err)
			}
		}
		// HTTP transport may already have removed previous_response_id from body.
		if _, err := NormalizeCodexLiteCompatibilityTools(body, false, []byte(`{"previous_response_id":"prior"}`)); err == nil {
			t.Fatal("original incremental context lost before validation")
		}
	}
}

func TestCodexReservedUnknownSchemaRejectedAndOrdinaryConstraintsRetained(t *testing.T) {
	for _, reserved := range []string{codexLiteImageNamespace, codexLiteWebNamespace} {
		changed, _ := sjson.Set(reserved, "tools.0.parameters.properties.extra.type", "string")
		body := []byte(`{"tools":[],"input":[]}`)
		body, _ = sjson.SetRawBytes(body, "tools.-1", []byte(changed))
		if _, err := NormalizeCodexLiteCompatibilityTools(body, false); err == nil || gjson.Get(err.Error(), "error.code").String() != "codex_reserved_tool_schema_conflict" {
			t.Fatalf("unknown reserved definition accepted: %v", err)
		}
	}
	ordinary := `{"type":"function","name":"resize","parameters":{"type":"object","properties":{"width":{"type":"integer","minimum":1,"maximum":5},"images":{"type":"array","maxItems":5,"items":{"type":"string"}}}}}`
	body := []byte(`{"tools":[],"input":[]}`)
	body, _ = sjson.SetRawBytes(body, "tools.-1", []byte(ordinary))
	got := mustNormalizeCodexLiteTools(t, body, false)
	if !codexToolJSONEqual(gjson.GetBytes(got, "input.0.tools.0").Raw, ordinary) {
		t.Fatal("ordinary user schema constraints changed")
	}
}

package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestModelSchemaTypesDoNotRewriteBusinessValues(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"Read","parameters":{"type":"OBJECT","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"kind":{"type":"STRING","default":"UPPER"},"config":{"type":"OBJECT","default":{"type":"CaseSensitive"},"enum":[{"type":"KeepThis"}],"const":{"type":"KeepThat"}},"large":{"type":"INTEGER","maximum":9007199254740993}},"additionalProperties":true}}]}]}`)
	actual := convertGeminiRequestToCodex("model", body, true, true)
	for path, expected := range map[string]string{"type": "object", "properties.kind.type": "string", "properties.config.default.type": "CaseSensitive", "properties.config.enum.0.type": "KeepThis", "properties.config.const.type": "KeepThat"} {
		if gjson.GetBytes(actual, "tools.0.parameters."+path).String() != expected {
			t.Fatalf("schema or business value %s was changed: %s", path, actual)
		}
	}
	if gjson.GetBytes(actual, "tools.0.parameters.properties.large.maximum").Raw != "9007199254740993" || !gjson.GetBytes(actual, "tools.0.parameters.additionalProperties").Bool() || !gjson.GetBytes(actual, "tools.0.parameters.$schema").Exists() {
		t.Fatalf("caller constraints were changed: %s", actual)
	}
}

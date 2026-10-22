package gemini

import (
	"context"
	"testing"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestModelInputPreservesExplicitTextContract(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"text":{"verbosity":"low","format":{"type":"json_schema","name":"caller_result","strict":false,"schema":{"type":"object","properties":{"large":{"type":"integer","maximum":9007199254740993}},"additionalProperties":false}}}}`)
	ctx := translatorcommon.WithModelInput(context.Background())
	actual := sdktranslator.TranslateRequestEnvelope(ctx, sdktranslator.FormatGemini, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Model: "model", Body: body, Stream: true})
	if gjson.GetBytes(actual.Body, "text").Raw != gjson.GetBytes(body, "text").Raw {
		t.Fatalf("explicit structured output contract was lost: %s", actual.Body)
	}
}

func TestModelSchemaTypesDoNotRewriteBusinessValues(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"Read","parameters":{"type":"OBJECT","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"kind":{"type":"STRING","default":"UPPER"},"config":{"type":"OBJECT","default":{"type":"CaseSensitive"},"enum":[{"type":"KeepThis"}],"const":{"type":"KeepThat"}},"large":{"type":"INTEGER","maximum":9007199254740993}},"additionalProperties":true}}]}]}`)
	actual, err := convertGeminiRequestToCodex("model", body, true, true)
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{"type": "object", "properties.kind.type": "string", "properties.config.default.type": "CaseSensitive", "properties.config.enum.0.type": "KeepThis", "properties.config.const.type": "KeepThat"} {
		if gjson.GetBytes(actual, "tools.0.parameters."+path).String() != expected {
			t.Fatalf("schema or business value %s was changed: %s", path, actual)
		}
	}
	if gjson.GetBytes(actual, "tools.0.parameters.properties.large.maximum").Raw != "9007199254740993" || !gjson.GetBytes(actual, "tools.0.parameters.additionalProperties").Bool() || !gjson.GetBytes(actual, "tools.0.parameters.$schema").Exists() {
		t.Fatalf("caller constraints were changed: %s", actual)
	}
}

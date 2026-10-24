package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func equalRuntimeNativeJSON(t *testing.T, expected, actual []byte) {
	t.Helper()
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if errDecode := decoder.Decode(&value); errDecode != nil {
			t.Fatalf("invalid native JSON: %s (%v)", raw, errDecode)
		}
		return value
	}
	if !reflect.DeepEqual(decode(expected), decode(actual)) {
		t.Fatalf("native JSON differs:\nexpected=%s\nactual=%s", expected, actual)
	}
}

func codexModelInputForSource(source sdktranslator.Format) []byte {
	switch source {
	case sdktranslator.FormatOpenAI, sdktranslator.FormatClaude:
		return []byte(`{"model":"caller-alias","messages":[{"role":"user","content":"hello"}]}`)
	case sdktranslator.FormatGemini:
		return []byte(`{"model":"caller-alias","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	default:
		return []byte(`{"model":"caller-alias","input":"hello"}`)
	}
}

func translateRuntimeModelInput(t *testing.T, source sdktranslator.Format, body []byte) []byte {
	t.Helper()
	e := &CodexExecutor{runtime: &CodexRuntimeExecutor{}}
	_, actual, _, errTranslate := e.translateRequestPair(source, sdktranslator.FormatCodex, "resolved-model", body, body, true, nil, false)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	return actual
}

func TestCodexRuntimePreservesUnconsumedModelFields(t *testing.T) {
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		t.Run(source.String(), func(t *testing.T) {
			body := codexModelInputForSource(source)
			// The adapter must not need a new supported-field entry for each key.
			for index := range 32 {
				field := fmt.Sprintf("native_extension_%d", index)
				body, _ = sjson.SetRawBytes(body, field, []byte(`{"large":9007199254740993,"nullable":null,"nested":{"enabled":false},"values":[]}`))
			}
			body, _ = sjson.SetRawBytes(body, "reasoning", []byte(`{"context":"all_turns","future_native_setting":{"enabled":false}}`))
			body, _ = sjson.SetRawBytes(body, "text", []byte(`{"verbosity":"low","future_native_setting":{"large":9007199254740993}}`))
			actual := translateRuntimeModelInput(t, source, body)
			gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
				if key.String() == "reasoning" || key.String() == "text" || strings.HasPrefix(key.String(), "native_extension_") {
					equalRuntimeNativeJSON(t, []byte(value.Raw), []byte(gjson.GetBytes(actual, key.String()).Raw))
				}
				return true
			})
			for _, consumed := range []string{"messages", "contents", "systemInstruction", "generationConfig"} {
				if gjson.GetBytes(actual, consumed).Exists() {
					t.Fatalf("source structure %s was restored: %s", consumed, actual)
				}
			}
			if len(gjson.GetBytes(actual, "input").Array()) == 0 {
				t.Fatalf("converted model input was lost: %s", actual)
			}
		})
	}
}

func TestCodexRuntimePreservesNativeToolStructures(t *testing.T) {
	tools := []byte(`[{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object","properties":{"kind":{"type":"string","const":"CaseSensitive"}},"additionalProperties":false},"future_declaration":{"large":9007199254740993}},{"type":"namespace","name":"warehouse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"defer_loading":true}],"description":"client tools"},{"type":"custom","name":"source","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"web_search","external_web_access":false,"search_content_types":["text","image"]},{"type":"tool_search","execution":"client"},{"type":"image_generation","model":"gpt-image-2"},{"type":"future_native_tool","native_options":{"preserved":true}}]`)
	choice := []byte(`{"type":"function","name":"lookup","namespace":"warehouse","future_choice":{"preserved":true}}`)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		t.Run(source.String(), func(t *testing.T) {
			body := codexModelInputForSource(source)
			body, _ = sjson.SetRawBytes(body, "tools", tools)
			body, _ = sjson.SetRawBytes(body, "tool_choice", choice)
			actual := translateRuntimeModelInput(t, source, body)
			equalRuntimeNativeJSON(t, tools, []byte(gjson.GetBytes(actual, "tools").Raw))
			equalRuntimeNativeJSON(t, choice, []byte(gjson.GetBytes(actual, "tool_choice").Raw))
		})
	}
}

func TestCodexRuntimeChatPreservesNativeContentAndCallFields(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","id":"msg_history","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"lookup","annotations":[],"future_part":false}],"tool_calls":[{"type":"function","id":"call_history","namespace":"warehouse","status":"completed","function":{"name":"lookup","arguments":"{}","future_call":{"large":9007199254740993}}}]},{"role":"tool","tool_call_id":"call_history","content":[{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"},{"type":"input_file","file_url":"https://example.invalid/file","filename":"source.txt"},{"type":"future_native_part","payload":{"preserved":true}}]}],"tools":[{"type":"function","defer_loading":true,"function":{"name":"lookup","strict":false,"parameters":{"type":"object"},"output_schema":{"type":"string"}}}]}`)
	// Chat's id/tool_call_id relationship owns the converted call_id fields.
	body, _ = sjson.SetBytes(body, "messages.0.tool_calls.0.call_id", "unrelated-metadata")
	body, _ = sjson.SetBytes(body, "messages.1.call_id", "unrelated-metadata")
	body, _ = sjson.SetBytes(body, "messages.1.type", "message")
	actual := translateRuntimeModelInput(t, sdktranslator.FormatOpenAI, body)
	for path, want := range map[string]string{
		"input.0.id": `"msg_history"`, "input.0.status": `"completed"`, "input.0.phase": `"commentary"`,
		"input.0.content.0.future_part": "false", "input.1.namespace": `"warehouse"`, "input.1.status": `"completed"`,
		"input.1.call_id": `"call_history"`, "input.1.future_call.large": "9007199254740993", "input.2.call_id": `"call_history"`,
		"tools.0.defer_loading": "true", "tools.0.output_schema.type": `"string"`,
	} {
		if got := gjson.GetBytes(actual, path).Raw; got != want {
			t.Fatalf("%s = %s, want %s; body=%s", path, got, want, actual)
		}
	}
	equalRuntimeNativeJSON(t, []byte(gjson.GetBytes(body, "messages.1.content").Raw), []byte(gjson.GetBytes(actual, "input.2.output").Raw))
}

func TestCodexRuntimePreservesNativeContentAcrossSourceProtocols(t *testing.T) {
	parts := []byte(`[{"type":"input_text","text":"hello","future_part":{"large":9007199254740993}},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"},{"type":"input_file","file_url":"https://example.invalid/file","filename":"source.txt"},{"type":"future_native_part","payload":{"preserved":true}}]`)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		t.Run(source.String(), func(t *testing.T) {
			body := codexModelInputForSource(source)
			path := "messages.0.content"
			if source == sdktranslator.FormatGemini {
				path = "contents.0.parts"
			} else if source == sdktranslator.FormatInteractions {
				body = []byte(`{"input":[{"type":"user_input","role":"user","content":[]}]}`)
				path = "input.0.content"
			}
			body, _ = sjson.SetRawBytes(body, path, parts)
			actual := translateRuntimeModelInput(t, source, body)
			var content [][]byte
			for _, item := range gjson.GetBytes(actual, "input").Array() {
				for _, part := range item.Get("content").Array() {
					content = append(content, []byte(part.Raw))
				}
			}
			if len(content) != len(gjson.ParseBytes(parts).Array()) {
				t.Fatalf("native parts were discarded: %s", actual)
			}
			for index, expected := range gjson.ParseBytes(parts).Array() {
				equalRuntimeNativeJSON(t, []byte(expected.Raw), content[index])
			}
		})
	}
}

func TestCodexRuntimePreservesNativeHistoryBeforeTypedParsing(t *testing.T) {
	history := []byte(`[{"type":"web_search_call","id":"ws_search","status":"completed","action":{"type":"search","query":"example","sources":[]}},{"type":"image_generation_call","id":"ig_example","status":"in_progress","result":null},{"type":"tool_search_output","id":"ts_example","status":"completed","tools":[]},{"type":"custom_tool_call","id":"ctc_example","call_id":"call_example","name":"source","input":"payload","status":"incomplete"},{"type":"custom_tool_call_output","call_id":"call_example","output":"result"},{"type":"future_native_history","future_native_state":{"large":9007199254740993}}]`)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, sdktranslator.FormatInteractions} {
		t.Run(source.String(), func(t *testing.T) {
			body, _ := sjson.SetRawBytes([]byte(`{}`), "input", history)
			actual := translateRuntimeModelInput(t, source, body)
			equalRuntimeNativeJSON(t, history, []byte(gjson.GetBytes(actual, "input").Raw))
		})
	}
}

func TestCodexRuntimeImageAdapterPreservesUnconsumedOptions(t *testing.T) {
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, form := range []bool{false, true} {
			if form && path == codexImagesGenerationsPath {
				continue
			}
			t.Run(fmt.Sprintf("%s/multipart=%t", path, form), func(t *testing.T) {
				body, contentType := runtimeImagePayload(t, form)
				if !form {
					body, _ = sjson.SetRawBytes(body, "future_image_option", []byte(`{"large":9007199254740993,"nullable":null,"enabled":false}`))
					body, _ = sjson.SetRawBytes(body, "quality", []byte("null"))
				}
				opts := codexOpenAIImageTestOptions(path, false)
				opts.Headers = http.Header{"Content-Type": {contentType}}
				prepared, errPrepare := codexPrepareOpenAIImageRequest(coreexecutor.Request{Model: "image-alias", Payload: body}, opts, true)
				if errPrepare != nil {
					t.Fatal(errPrepare)
				}
				tool := gjson.GetBytes(prepared.Body, "tools.0")
				if form {
					equalRuntimeNativeJSON(t, []byte(`["one","two"]`), []byte(tool.Get("extra").Raw))
					if tool.Get("n").Int() != 2 {
						t.Fatalf("multipart business option was discarded: %s", tool.Raw)
					}
				} else {
					equalRuntimeNativeJSON(t, []byte(gjson.GetBytes(body, "future_image_option").Raw), []byte(tool.Get("future_image_option").Raw))
					if tool.Get("quality").Raw != "null" {
						t.Fatalf("explicit image option was replaced: %s", tool.Raw)
					}
				}
				for _, consumed := range []string{"prompt", "images", "mask", "response_format", "stream"} {
					if tool.Get(consumed).Exists() {
						t.Fatalf("image source structure %s was restored: %s", consumed, tool.Raw)
					}
				}
				if path == codexImagesEditsPath && !tool.Get("input_image_mask.image_url").Exists() {
					t.Fatalf("image mask conversion was lost: %s", prepared.Body)
				}
			})
		}
	}
}

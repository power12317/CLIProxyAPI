package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func testNativeRuntimeCompatibleInputs(t *testing.T, cfg *config.Config, auth *coreauth.Auth, home, transport string, readExchange func(*testing.T) nativeRuntimeExchange) {
	t.Helper()
	schema := `{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"path":{"type":"string","pattern":"\\p{L}+","default":"home"},"count":{"type":"integer","minimum":0},"items":{"type":"array","maxItems":8}},"required":["path"],"additionalProperties":false}`
	tools := []byte(`[{"type":"function","name":"Read","parameters":` + schema + `,"strict":false,"output_schema":{"type":"string"}},{"type":"function","name":"Bash"},{"type":"function","name":"Agent"},{"type":"function","name":"WebSearch"},{"type":"function","name":"ToolSearch"},{"type":"namespace","name":"web","tools":[{"type":"function","name":"run","parameters":{"type":"object","properties":{"query":{"type":"string"}}},"strict":false}]}]`)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("compatible_native_controls/%s/stream=%t", source, stream), func(t *testing.T) {
				body := []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"compatible business input","status":"displayed","metadata":{"ui":true}}],"tools":` + string(tools) + `,"tool_choice":"none","parallel_tool_calls":false,"include":[],"store":false,"background":false,"temperature":0,"top_p":0.7,"max_output_tokens":120,"service_tier":"flex","stream_options":{"include_usage":true},"client_metadata":{"optional_trace":{"client":"WorkBuddy"},"ws_request_header_x_openai_internal_codex_responses_lite":"false","x-codex-client-timing":{"old":true}},"text":{"verbosity":"low","format":{"type":"json_schema","name":"inventory_contract","strict":false,"schema":` + schema + `}}}`)
				if source != sdktranslator.FormatOpenAIResponse {
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"compatible business input"}]`))
				}
				if source == sdktranslator.FormatClaude {
					body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"none"}`))
					body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"name":"Read","input_schema":`+schema+`,"strict":false,"output_schema":{"type":"string"}},{"name":"Bash"},{"name":"Agent"},{"name":"WebSearch"},{"name":"ToolSearch"}]`))
					body, _ = sjson.SetRawBytes(body, "output_config.format", []byte(`{"type":"json_schema","name":"inventory_contract","strict":false,"schema":`+schema+`}`))
				}
				opts := runtimeTestOptions()
				opts.SourceFormat = source
				output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, opts, stream)
				if err != nil || !strings.Contains(string(output), "native result") {
					t.Fatalf("native input was rejected or response lost: err=%v output=%s", err, output)
				}
				actual := readExchange(t)
				for key, expected := range map[string]string{"tool_choice": `"none"`, "parallel_tool_calls": "false", "include": "[]", "store": "false", "temperature": "0", "top_p": "0.7", "max_output_tokens": "120", "service_tier": `"flex"`} {
					t.Run(key, func(t *testing.T) {
						equalRuntimeJSON(t, []byte(expected), []byte(gjson.GetBytes(actual.body, key).Raw))
					})
				}
				if gjson.GetBytes(actual.body, "text.format.name").String() != "inventory_contract" || gjson.GetBytes(actual.body, "text.format.strict").Type != gjson.False {
					t.Fatalf("caller structured output was replaced: %s", actual.body)
				}
				equalRuntimeJSON(t, []byte(schema), []byte(gjson.GetBytes(actual.body, "text.format.schema").Raw))
				declarations := nativeRuntimeToolDeclarations(actual.body)
				for _, name := range []string{"Read", "Bash", "Agent", "WebSearch", "ToolSearch"} {
					if declarations[name].Get("type").String() != "function" {
						t.Fatalf("third-party function %s was replaced or omitted: %s", name, actual.body)
					}
				}
				read := declarations["Read"]
				if read.Get("strict").Type != gjson.False || read.Get("output_schema.type").String() != "string" {
					t.Fatalf("caller tool controls were lost: %s", read.Raw)
				}
				equalRuntimeJSON(t, []byte(schema), []byte(read.Get("parameters").Raw))
				if source != sdktranslator.FormatClaude && declarations["web"].Get("tools.0.name").String() != "run" {
					t.Fatalf("namespace declaration was replaced: %s", actual.body)
				}
				if bytes := gjson.GetBytes(actual.body, "client_metadata.x-codex-client-timing"); bytes.Get("old").Bool() {
					t.Fatalf("stale caller timing was copied as native timing: %s", actual.body)
				}
			})
		}
	}
	t.Run("native_generate_false", func(t *testing.T) {
		body := []byte(`{"model":"gpt-6.1-sol","input":"prewarm only","generate":false}`)
		_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, runtimeTestOptions(), false)
		if transport == "http" {
			if err == nil || !strings.Contains(err.Error(), "generate=false") {
				t.Fatalf("HTTP generated a model response for generate=false: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		actual := readExchange(t)
		if gjson.GetBytes(actual.body, "generate").Type != gjson.False {
			t.Fatalf("prewarm was changed into generation: %s", actual.body)
		}
	})
	t.Run("third_party_tool_only_completion", func(t *testing.T) {
		body := []byte(`{"model":"gpt-6.1-sol","input":"third-party-tool-fixture","tools":[{"type":"function","name":"query_inventory","parameters":` + schema + `,"strict":false},{"type":"custom","name":"run_source","format":{"type":"text"}}]}`)
		output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, runtimeTestOptions(), true)
		if err != nil || !strings.Contains(string(output), `"call_id":"call_inventory"`) || !strings.Contains(string(output), `"name":"run_source"`) || !strings.Contains(string(output), "response.completed") {
			t.Fatalf("caller tool-only result was not delivered: err=%v output=%s", err, output)
		}
		readExchange(t)
		if _, err := os.Stat(filepath.Join(home, "must-not-be-created")); !os.IsNotExist(err) {
			t.Fatalf("client tool ran in the server: %v", err)
		}
	})
}

func nativeRuntimeToolDeclarations(body []byte) map[string]gjson.Result {
	declarations := map[string]gjson.Result{}
	var add func(gjson.Result)
	add = func(tools gjson.Result) {
		for _, tool := range tools.Array() {
			declarations[tool.Get("name").String()] = tool
			if tool.Get("type").String() == "namespace" && tool.Get("name").String() == "functions" {
				add(tool.Get("tools"))
			}
		}
	}
	add(gjson.GetBytes(body, "tools"))
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			add(item.Get("tools"))
		}
	}
	return declarations
}

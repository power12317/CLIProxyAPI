package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexRuntimeCompatibleBusinessInput(t *testing.T) {
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatInteractions} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", source, stream), func(t *testing.T) {
				captured := make(chan bridge.Request, 1)
				settings, auth := runtimeTestMaster(t, "compatibility.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
					captured <- req
					runtimeTestAccept(conn, rpc, req)
					runtimeTestFinish(conn, req, runtimeTerminal)
				})
				e := runtimeTestExecutor(settings)
				name := "query_inventory_" + strings.Repeat("x", 80)
				schema := `{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"code":{"type":"string","pattern":"\\p{L}+"},"limit":{"type":"integer","minimum":0,"default":3},"list":{"type":"array","maxItems":7}},"required":["code"],"additionalProperties":false}`
				tool := []byte(`{"type":"function","name":"name","parameters":` + schema + `,"strict":false,"output_schema":{"type":"string"}}`)
				tool, _ = sjson.SetBytes(tool, "name", name)
				body := []byte(`{"model":"runtime-model","input":"hello","tool_choice":"none","parallel_tool_calls":false,"include":[],"store":false,"background":false,"generate":true,"service_tier":"flex","max_output_tokens":25,"prompt_cache_retention":"24h","stream_options":{"include_usage":true},"client_metadata":{"optional_trace":{"name":"WorkBuddy"},"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
				body, _ = sjson.SetRawBytes(body, "text", []byte(`{"format":{"type":"json_schema","name":"source_result","strict":false,"schema":{"type":"string"}}}`))
				if source == sdktranslator.FormatOpenAI {
					function, _ := sjson.DeleteBytes(tool, "type")
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"hello"}]`))
					tool = append(append([]byte(`{"type":"function","function":`), function...), '}')
				}
				if source == sdktranslator.FormatClaude {
					body, _ = sjson.SetRawBytes(body, "output_config.format", []byte(gjson.GetBytes(body, "text.format").Raw))
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"hello"}]`))
					body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"none"}`))
					tool, _ = sjson.SetRawBytes(tool, "input_schema", []byte(schema))
					tool, _ = sjson.DeleteBytes(tool, "parameters")
				}
				if source == sdktranslator.FormatGemini {
					body, _ = sjson.DeleteBytes(body, "input")
					body, _ = sjson.SetRawBytes(body, "contents", []byte(`[{"role":"user","parts":[{"text":"hello"}]}]`))
					tool = append(append([]byte(`{"functionDeclarations":[`), tool...), ']', '}')
				}
				body, _ = sjson.SetRawBytes(body, "tools", append(append([]byte{'['}, tool...), ']'))
				opts := runtimeTestOptions()
				opts.SourceFormat, opts.Stream = source, stream
				req := coreexecutor.Request{Model: "runtime-model", Payload: body}
				ctx := t.Context()
				if stream {
					result, err := e.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				actual := (<-captured).Request
				for _, field := range []string{"tool_choice", "parallel_tool_calls", "include", "store", "background", "generate", "service_tier", "max_output_tokens", "prompt_cache_retention", "stream_options", "client_metadata.optional_trace", "text"} {
					if source == sdktranslator.FormatClaude && field == "tool_choice" {
						continue
					}
					equalRuntimeJSON(t, []byte(gjson.GetBytes(body, field).Raw), []byte(gjson.GetBytes(actual, field).Raw))
				}
				if gjson.GetBytes(actual, "tool_choice").String() != "none" {
					t.Fatal("source tool choice was not mapped to none")
				}
				if gjson.GetBytes(actual, "tools.#").Int() != 1 || gjson.GetBytes(actual, "tools.0.name").String() != name || gjson.GetBytes(actual, "tools.0.strict").Type != gjson.False || gjson.GetBytes(actual, "tools.0.output_schema.type").String() != "string" {
					t.Fatalf("caller declaration was replaced or renamed: %s", actual)
				}
				equalRuntimeJSON(t, []byte(schema), []byte(gjson.GetBytes(actual, "tools.0.parameters").Raw))
			})
		}
	}
}

func TestCodexRuntimeChatCallRelationships(t *testing.T) {
	for _, test := range []struct {
		name, messages, failure string
	}{
		{"named", `[{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_a","content":"done"}]`, ""},
		{"unique_missing_result_id", `[{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","content":"done"}]`, ""},
		{"unique_missing_pair_ids", `[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","content":"done"}]`, ""},
		{"consumed_before_inference", `[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"Read","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"Bash","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"first"},{"role":"tool","content":"second"}]`, ""},
		{"ambiguous", `[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"Read"}},{"id":"b","type":"function","function":{"name":"Bash"}}]},{"role":"tool","content":"done"}]`, "messages[1].tool_call_id cannot be inferred"},
		{"orphan", `[{"role":"tool","tool_call_id":"unknown","content":"done"}]`, "messages[0].tool_call_id has no matching"},
		{"duplicate", `[{"role":"assistant","tool_calls":[{"id":"same","type":"function","function":{"name":"Read"}},{"id":"same","type":"function","function":{"name":"Bash"}}]}]`, "messages[0].tool_calls[1].id is ambiguous"},
		{"missing_name", `[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"arguments":"{}"}}]}]`, "messages[0].tool_calls[0].function.name is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"messages":` + test.messages + `}`)
			err := helps.ValidateCodexRuntimeChatCalls(body)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error=%v, want %q", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			e := &CodexExecutor{runtime: &CodexRuntimeExecutor{}}
			_, actual, _, errTranslate := e.translateRequestPair(sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "runtime-model", body, body, false, nil, false)
			if errTranslate != nil {
				t.Fatal(errTranslate)
			}
			calls := map[string]string{}
			for _, item := range gjson.GetBytes(actual, "input").Array() {
				switch item.Get("type").String() {
				case "function_call":
					calls[item.Get("call_id").String()] = item.Get("name").String()
				case "function_call_output":
					if calls[item.Get("call_id").String()] == "" {
						t.Fatalf("tool result was assigned to an unknown call: %s", actual)
					}
					delete(calls, item.Get("call_id").String())
				}
			}
			if len(calls) != 0 {
				t.Fatalf("source tool call or result was lost: %s", actual)
			}
		})
	}
}

func TestCodexRuntimeTranslationDefaultsAndIdempotence(t *testing.T) {
	e := &CodexExecutor{runtime: &CodexRuntimeExecutor{}}
	body := []byte(`{"input":[{"role":"system","content":"policy"},{"role":"user","content":"question"}],"tools":[{"type":"function","name":"Read","input_schema":{"type":"object","properties":{}}}],"include":[],"parallel_tool_calls":false}`)
	_, first, _, errTranslate := e.translateRequestPair(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, "runtime-model", body, body, true, nil, false)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	_, second, _, errTranslate := e.translateRequestPair(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, "runtime-model", first, first, true, nil, false)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	equalRuntimeJSON(t, first, second)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
		input := []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","name":"Read"}]}`)
		_, actual, _, errTranslate := e.translateRequestPair(source, sdktranslator.FormatCodex, "runtime-model", input, input, true, nil, false)
		if errTranslate != nil {
			t.Fatal(errTranslate)
		}
		if gjson.GetBytes(actual, "reasoning.effort").Exists() || gjson.GetBytes(actual, "include").Exists() {
			t.Fatalf("%s imposed Agent defaults on missing controls: %s", source, actual)
		}
		if gjson.GetBytes(actual, "tools.#").Int() != 1 || gjson.GetBytes(actual, "tools.0.name").String() != "Read" || gjson.GetBytes(actual, "tools.0.strict").Type != gjson.False {
			t.Fatalf("%s lost the caller's parameterless declaration: %s", source, actual)
		}
	}
	input := []byte(`{"messages":[{"role":"user","content":"hello"}],"tool_choice":{"type":"none"}}`)
	_, actual, _, errTranslate := e.translateRequestPair(sdktranslator.FormatClaude, sdktranslator.FormatCodex, "runtime-model", input, input, true, nil, false)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	if gjson.GetBytes(actual, "tool_choice").String() != "none" {
		t.Fatalf("explicit Claude tool choice was lost without declarations: %s", actual)
	}
	for _, format := range []string{`"response_format":{"type":"json_object"}`, `"response_format":{"type":"json_schema","json_schema":{"name":"client_result","strict":false,"schema":{"type":"string"}}}`, `"text":{"format":{"type":"json_schema","name":"client_result","strict":false,"schema":{"type":"string"}}}`} {
		body := []byte(`{"messages":[{"role":"user","content":"hello"}],` + format + `}`)
		_, actual, _, errTranslate := e.translateRequestPair(sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "runtime-model", body, body, true, nil, false)
		if errTranslate != nil {
			t.Fatal(errTranslate)
		}
		if strings.Contains(format, "json_object") {
			if gjson.GetBytes(actual, "text.format.type").String() != "json_object" {
				t.Fatalf("Chat JSON-object format was lost: %s", actual)
			}
		} else if gjson.GetBytes(actual, "text.format.name").String() != "client_result" || gjson.GetBytes(actual, "text.format.strict").Type != gjson.False || gjson.GetBytes(actual, "text.format.schema.type").String() != "string" {
			t.Fatalf("Chat structured output contract was lost: %s", actual)
		}
	}
}

func TestCodexRuntimePayloadRulesFollowCompatibility(t *testing.T) {
	captured := make(chan bridge.Request, 1)
	settings, auth := runtimeTestMaster(t, "payload.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
		captured <- req
		runtimeTestAccept(conn, rpc, req)
		runtimeTestFinish(conn, req, runtimeTerminal)
	})
	e := runtimeTestExecutor(settings)
	e.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "runtime-model"}}, Params: map[string]any{"parallel_tool_calls": false, "tool_choice": "none", "include": []any{}}}}
	e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "runtime-model"}}, Params: []string{"tools", "client_metadata.optional_trace"}}}
	body := []byte(`{"input":"hello","tools":[{"type":"namespace","name":"web","tools":[{"type":"function","name":"run","parameters":{"type":"object","properties":{"private":{"type":"string"}}}}]}],"parallel_tool_calls":true,"client_metadata":{"optional_trace":"remove"}}`)
	if _, err := e.Execute(context.Background(), auth, coreexecutor.Request{Model: "runtime-model", Payload: body}, runtimeTestOptions()); err != nil {
		t.Fatal(err)
	}
	actual := (<-captured).Request
	if gjson.GetBytes(actual, "tools").Exists() || gjson.GetBytes(actual, "client_metadata.optional_trace").Exists() || gjson.GetBytes(actual, "tool_choice").String() != "none" || gjson.GetBytes(actual, "parallel_tool_calls").Type != gjson.False || gjson.GetBytes(actual, "include").Raw != "[]" {
		t.Fatalf("final payload semantics were rewritten: %s", actual)
	}
}

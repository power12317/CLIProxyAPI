package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDelegationContextPreservesCompleteHistory(t *testing.T) {
	const output = " <codex_delegation>\n  <input>Keep quotes \" and \\ paths, 中文, and all handoff text.</input>\n</codex_delegation>\n"
	for _, name := range []string{"create_thread", "send_message_to_thread"} {
		body, _ := decode([]byte(testRequest))
		body["input"] = []any{
			message("user", "Continue the handed-off task"),
			object{"type": "function_call_output", "id": "fco_handoff", "namespace": "codex_app", "name": name, "output": output},
			object{"type": "function_call", "call_id": "call_weather", "name": "weather", "arguments": `{"city":"Tokyo"}`},
			object{"type": "function_call_output", "call_id": "call_weather", "output": "18C"},
		}
		raw, _ := json.Marshal(body)
		original := bytes.Clone(raw)
		cache := &Cache{}
		for _, scope := range []string{"account-a", "account-a", "account-b"} {
			wire, _, err := Prepare(raw, scope, "session", cache)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, scope, err)
			}
			items := gjson.GetBytes(wire, "input").Array()
			if len(items) != 5 || items[2].Get("role").String() != "user" || items[2].Get("type").String() != "message" || items[2].Get("content.0.text").String() != "Tool output from codex_app__"+name+":\n"+output {
				t.Fatalf("handoff content or position changed: %s", wire)
			}
			if items[3].Get("call_id").String() != "call_weather" || items[3].Get("name").String() != "run_officejs" || items[4].Get("type").String() != "function_call_output" || items[4].Get("output").String() != "18C" {
				t.Fatal("ordinary tool history changed")
			}
			if gjson.GetBytes(wire, "metadata.agent_iteration").String() != "2" {
				t.Fatal("handoff was counted as an executed tool")
			}
		}
		if !bytes.Equal(raw, original) {
			t.Fatal("caller payload mutated")
		}
	}
}

func TestDelegationContextLeavesPairedAndUnrelatedResultsUntouched(t *testing.T) {
	original := object{"type": "function_call_output", "id": "fco_handoff", "namespace": "codex_app", "name": "create_thread", "output": "<codex_delegation>exact context</codex_delegation>"}
	for _, patch := range []object{
		{"call_id": "paired-call"}, {"namespace": "other"}, {"name": "other"},
		{"output": "ordinary tool output"}, {"type": "custom_tool_call_output"},
	} {
		item := clone(original)
		for key, value := range patch {
			item[key] = value
		}
		before := clone(item)
		if _, ok := delegationContext(item); ok {
			t.Fatalf("unrelated tool result rewritten: %#v", patch)
		}
		if !reflect.DeepEqual(item, before) {
			t.Fatal("input item mutated")
		}
	}
	body, _ := decode([]byte(testRequest))
	paired := clone(original)
	paired["call_id"] = "paired-call"
	body["input"] = []any{message("user", "Continue"), object{"type": "function_call", "call_id": "paired-call", "namespace": "codex_app", "name": "create_thread", "arguments": "{}"}, paired}
	raw, _ := json.Marshal(body)
	wire, _, err := Prepare(raw, "account", "session", &Cache{})
	if err != nil || gjson.GetBytes(wire, "input.3.type").String() != "function_call_output" || gjson.GetBytes(wire, "input.3.output").String() != original["output"] {
		t.Fatalf("paired result changed: %v %s", err, wire)
	}
}

func TestCustomExecCommandObjectsBecomeClientPrograms(t *testing.T) {
	command := object{"cmd": "printf '%s\\n' \"quoted\"; echo '$HOME'; echo '\\d+'", "workdir": "/tmp/path with spaces/中文", "yield_time_ms": json.Number("10000"), "max_output_tokens": json.Number("12000"), "login": false}
	for _, field := range []string{"input", "args", "arguments", "direct"} {
		cache := &Cache{}
		wire, bridge, err := Prepare(catalogRequest(), "account-a", "session", cache)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gjson.GetBytes(wire, "input.0.content.0.text").String(), "const result = await tools.exec_command") {
			t.Fatal("upstream catalog lacks executable custom example")
		}
		native := nativeWeather()
		native["call_id"] = "call_exec"
		if field == "direct" {
			args, _ := json.Marshal(command)
			native["name"], native["arguments"] = "functions.exec", string(args)
		} else {
			envelope, _ := json.Marshal(object{"name": "functions.exec", field: command})
			args, _ := json.Marshal(object{"summary": "Inspect project and apps", "code": string(envelope)})
			native["arguments"] = string(args)
		}
		before := clone(native)
		converted, err := bridge.convertItem(native)
		if err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		if converted["type"] != "custom_tool_call" || converted["name"] != "exec" || converted["namespace"] != "functions" {
			t.Fatalf("wrong client tool: %#v", converted)
		}
		program := stringValue(converted["input"])
		const prefix = "const result = await tools.exec_command("
		const suffix = "); text(result);"
		if !strings.HasPrefix(program, prefix) || !strings.HasSuffix(program, suffix) {
			t.Fatalf("not an exec_command invocation: %s", program)
		}
		got, errDecode := decode([]byte(strings.TrimSuffix(strings.TrimPrefix(program, prefix), suffix)))
		if errDecode != nil || !reflect.DeepEqual(got, command) {
			t.Fatalf("parameters changed: %#v %v", got, errDecode)
		}
		if !reflect.DeepEqual(before, native) {
			t.Fatal("native response mutated")
		}
		if field != "direct" && !reflect.DeepEqual(cache.get(bridge.scope+"/call_exec"), native) {
			t.Fatal("native replay item changed")
		}
		body, _ := decode(catalogRequest())
		body["input"] = append(body["input"].([]any), converted, object{"type": "custom_tool_call_output", "call_id": "call_exec", "output": "client result"})
		raw, _ := json.Marshal(body)
		for _, scope := range []string{"account-a", "account-b"} {
			replayed, _, errReplay := Prepare(raw, scope, "session", cache)
			if errReplay != nil {
				t.Fatalf("%s/%s: %v", field, scope, errReplay)
			}
			if gjson.GetBytes(replayed, "input.3.output").String() != "client result" {
				t.Fatal("tool result lost across accounts")
			}
			if scope == "account-b" {
				args := gjson.Parse(gjson.GetBytes(replayed, "input.2.arguments").String())
				if gjson.Parse(args.Get("code").String()).Get("input").String() != program {
					t.Fatal("rebuilt custom input changed")
				}
			}
		}
	}
}

func TestCustomInputAliasesPreserveRawText(t *testing.T) {
	for _, name := range []string{"exec", "patch"} {
		spec := tool{Name: name, Namespace: "functions", Type: "custom"}
		for _, raw := range []string{"await tool({text: '\\d+ \\\"'});\n", `{"cmd":"this is raw custom text"}`, ""} {
			for _, field := range []string{"input", "args", "arguments"} {
				got, err := customToolInput(spec, object{field: raw}, false)
				if err != nil || got != raw {
					t.Fatalf("%s/%s changed raw input: %q %v", name, field, got, err)
				}
			}
		}
	}
	for _, envelope := range []object{{"input": "one", "arguments": "two"}, {"input": "one", "args": "two"}} {
		if _, err := customToolInput(tool{Name: "exec", Namespace: "functions", Type: "custom"}, envelope, false); err == nil {
			t.Fatal("conflicting aliases accepted")
		}
	}
	if _, err := customToolInput(tool{Name: "patch", Namespace: "functions", Type: "custom"}, object{"arguments": object{"cmd": "pwd"}}, false); err == nil {
		t.Fatal("unrelated custom tool treated as exec")
	}
}

func TestCustomExecConversionPreservesStreamingProgress(t *testing.T) {
	_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	native := nativeWeather()
	code, _ := json.Marshal(object{"name": "functions.exec", "arguments": object{"cmd": "pwd", "workdir": "/tmp"}})
	args, _ := json.Marshal(object{"summary": "Inspect files", "code": string(code)})
	native["arguments"] = string(args)
	var upstream bytes.Buffer
	for _, event := range []object{
		{"type": "response.created", "response": object{"status": "in_progress"}},
		{"type": "response.in_progress", "response": object{"status": "in_progress"}},
		{"type": "response.completed", "response": object{"status": "completed", "output": []any{native}}},
	} {
		raw, _ := json.Marshal(event)
		upstream.WriteString("data: " + string(raw) + "\n\n")
	}
	var types []string
	var downstream bytes.Buffer
	err = bridge.Stream(&upstream, func(raw []byte) error {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "data:") {
				event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				types = append(types, event.Get("type").String())
			}
		}
		downstream.Write(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) < 3 || types[0] != "response.created" || types[1] != "response.in_progress" || types[len(types)-1] != "response.completed" {
		t.Fatalf("stream order changed: %v", types)
	}
	if !strings.Contains(downstream.String(), `"type":"custom_tool_call"`) || strings.Contains(downstream.String(), "response.failed") {
		t.Fatal("custom tool stream failed")
	}
}

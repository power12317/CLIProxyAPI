package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestUnmarkedCustomExecPreservesEntireProgram(t *testing.T) {
	programs := []string{
		`const matches = ALL_TOOLS.filter(x => /description|thread.*update|thread.*title/i.test(x.name+" "+x.description)); return matches;`,
		`await tools.exec_command({cmd: 'pwd'});`,
		"// @exec: {\"max_output_tokens\": 1000}\r\nconst result = await tools.exec_command({cmd: 'pwd'}); text(result);\n",
		"/* Keep all calls and comments. */\nconst results = await Promise.all([tools.first({}), tools.second({})]); text(results);\n",
		`const metadata = {"name":"functions.shell","arguments":{"cmd":"data only"}}; text(metadata); await tools.exec_command({cmd: 'pwd'});`,
		`text(ALL_TOOLS.filter(x => /tool/.test(x.name)));`,
		`tools["exec_command"]({cmd: 'pwd'}).then(text);`,
	}
	for _, program := range programs {
		for depth := 0; depth <= 2; depth++ {
			cache := &Cache{}
			_, bridge, err := Prepare(catalogRequest(), "account-a", "session", cache)
			if err != nil {
				t.Fatal(err)
			}
			arguments := object{"summary": "Inspect available tools", "code": program}
			for i := 0; i < depth; i++ {
				code, _ := json.Marshal(object{"name": "functions.run_officejs", "arguments": arguments})
				arguments = object{"summary": "Relay client tool", "code": string(code)}
			}
			encoded, _ := json.Marshal(arguments)
			native := nativeWeather()
			native["arguments"] = string(encoded)
			before := clone(native)
			converted, err := bridge.convertItem(native)
			if err != nil {
				t.Fatalf("depth=%d: %v", depth, err)
			}
			if converted["type"] != "custom_tool_call" || converted["namespace"] != "functions" || converted["name"] != "exec" || converted["input"] != program {
				t.Fatalf("client program changed: %#v", converted)
			}
			if !reflect.DeepEqual(native, before) || !reflect.DeepEqual(cache.get(bridge.scope+"/call_weather"), before) {
				t.Fatal("original upstream replay item changed")
			}
			body, _ := decode(catalogRequest())
			body["input"] = append(body["input"].([]any), converted, object{"type": "custom_tool_call_output", "call_id": "call_weather", "output": "client result"})
			raw, _ := json.Marshal(body)
			for _, scope := range []string{"account-a", "account-b"} {
				wire, _, errReplay := Prepare(raw, scope, "session", cache)
				if errReplay != nil || gjson.GetBytes(wire, "input.3.output").String() != "client result" {
					t.Fatalf("replay failed: %s %v", scope, errReplay)
				}
				if scope == "account-b" {
					args := gjson.Parse(gjson.GetBytes(wire, "input.2.arguments").String())
					if gjson.Parse(args.Get("code").String()).Get("input").String() != program {
						t.Fatal("rebuilt client program changed across accounts")
					}
				}
			}
		}
	}
}

func TestUnmarkedCustomExecRequiresDeclaredRuntime(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools map[string]tool
		code  any
	}{
		{"missing exec", map[string]tool{}, `await tools.exec_command({cmd: 'pwd'});`},
		{"function exec", map[string]tool{"functions.exec": {Type: "function", Namespace: "functions", Name: "exec"}}, `text(ALL_TOOLS);`},
		{"unrelated exec", map[string]tool{"exec": {Type: "custom", Name: "exec", Description: "Run shell commands"}}, `text(ALL_TOOLS);`},
		{"ambiguous exec", map[string]tool{"functions.exec": {Type: "custom", Namespace: "functions", Name: "exec"}, "exec": {Type: "custom", Name: "exec", Description: "Run JavaScript"}}, `text(ALL_TOOLS);`},
		{"truncated JSON", nil, `{"name":"functions.exec","input":"text(ALL_TOOLS)"`},
		{"JSON array", nil, `[{"name":"functions.exec","input":"text(ALL_TOOLS)"}]`},
		{"JSON string", nil, `"text(ALL_TOOLS)"`},
		{"unknown wrapper", nil, `unknown_tool({"name":"functions.exec","input":"text(ALL_TOOLS)"})`},
		{"non-string", nil, 42},
		{"empty", nil, ""},
		{"prose", nil, "Use tools.exec_command to read the files."},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			if test.tools != nil {
				bridge.tools = test.tools
			}
			args, _ := json.Marshal(object{"summary": "Inspect client tools", "code": test.code})
			native := nativeWeather()
			native["arguments"] = string(args)
			if _, err = bridge.convertItem(native); err == nil {
				t.Fatal("unidentified runtime or non-program accepted")
			}
		})
	}
}

func TestUnmarkedCustomExecRespectsToolChoice(t *testing.T) {
	for _, choice := range []string{"auto", "required", "functions.exec", "functions.shell", "none"} {
		body, _ := decode(catalogRequest())
		body["tool_choice"] = choice
		raw, _ := json.Marshal(body)
		_, bridge, err := Prepare(raw, "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(object{"code": "text(ALL_TOOLS);"})
		native := nativeWeather()
		native["arguments"] = string(args)
		_, err = bridge.convertItem(native)
		if choice == "none" || choice == "functions.shell" {
			if err == nil || !strings.Contains(err.Error(), "unexpected_tool") {
				t.Fatalf("tool choice %s ignored: %v", choice, err)
			}
		} else if err != nil {
			t.Fatalf("declared exec rejected: %s %v", choice, err)
		}
	}
}

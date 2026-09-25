package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestToolEnvelopeFormattingVariants(t *testing.T) {
	const args = `{"cmd":"one\ntwo","pattern":"\\d+","n":9007199254740993,"literal":"\\n"}`
	plain := `{"name":"functions.shell","arguments":` + args + "}"
	quoted, _ := json.Marshal(plain)
	fence := strings.Repeat("\x60", 3)
	cases := map[string]any{
		"plain":                   plain,
		"object":                  object{"name": "functions.shell", "arguments": mustEnvelopeObject(t, args)},
		"double encoded":          string(quoted),
		"labeled object":          "Here is the tool request:\n" + plain,
		"labeled fence":           "Tool request:\n" + fence + "json\n" + plain + "\n" + fence,
		"CRLF fence":              fence + "json\r\n" + plain + "\r\n" + fence,
		"uppercase fence":         fence + "JSON\n" + plain + "\n" + fence,
		"raw newline":             strings.Replace(plain, `one\ntwo`, "one\ntwo", 1),
		"illegal escape":          strings.Replace(plain, `\\d+`, `\d+`, 1),
		"call wrapper":            "await functions.shell(" + plain + ");",
		"direct argument wrapper": "functions.shell(" + args + ")",
		"assignment wrapper":      "const request = " + plain + ";",
		"assignment with escape":  "const request = " + strings.Replace(plain, `\\d+`, `\d+`, 1) + ";",
		"prose after envelope":    "Tool request: " + plain + "\nEnd of request.",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			native := nativeWeather()
			outer, _ := json.Marshal(object{"code": value, "summary": "Run client tool"})
			native["arguments"] = string(outer)
			converted, err := bridge.convertItem(native)
			if err != nil {
				t.Fatal(err)
			}
			if converted["name"] != "shell" || converted["namespace"] != "functions" || !reflect.DeepEqual(mustEnvelopeObject(t, stringValue(converted["arguments"])), mustEnvelopeObject(t, args)) {
				t.Fatalf("tool identity or exact arguments changed: %#v", converted)
			}
			if !reflect.DeepEqual(bridge.cache.get(bridge.scope+"/call_weather"), native) {
				t.Fatal("formatting recovery changed the original replay item")
			}
		})
	}
}

func TestToolEnvelopeRepairsOuterAndFunctionArguments(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := nativeWeather()
		native["name"], native["arguments"] = "functions.shell", "{\"cmd\":\"one\ntwo\tthree\rfour\",\"pattern\":\"\\d+\"}"
		if wrapped {
			outer, _ := json.Marshal(object{"code": object{"name": "functions.shell", "arguments": native["arguments"]}, "summary": "one\ntwo"})
			native["name"], native["arguments"] = "run_officejs", strings.Replace(string(outer), `one\ntwo`, "one\ntwo", 1)
		}
		converted, err := bridge.convertItem(native)
		if err != nil {
			t.Fatal(err)
		}
		got := mustEnvelopeObject(t, stringValue(converted["arguments"]))
		if got["cmd"] != "one\ntwo\tthree\rfour" || got["pattern"] != `\d+` {
			t.Fatalf("arguments changed: %#v", got)
		}
	}
}

func TestToolEnvelopePreservesValidEscapesAndUnicode(t *testing.T) {
	const raw = `{"pattern":"\\d+\\s","path":"C:\\Projects\\file","newline":"a\nb","literal":"\\n","quote":"\"","unicode":"\u4e2d\ud83d\ude00","number":9007199254740993}`
	got, err := toolObject(raw)
	if err != nil || !reflect.DeepEqual(got, mustEnvelopeObject(t, raw)) {
		t.Fatalf("valid JSON changed: %#v, %v", got, err)
	}
	for _, invalid := range []string{`\d+\s`, `\uXYZZ`, `\u12`} {
		got, err := toolObject(`{"value":"` + invalid + `","valid":"\n","literal":"\\n"}`)
		if err != nil || got["value"] != invalid || got["valid"] != "\n" || got["literal"] != `\n` {
			t.Fatalf("escape recovery changed content: %#v, %v", got, err)
		}
	}
}

func TestNestedCustomTransportKeepsExactInput(t *testing.T) {
	const input = "await tools.exec_command({cmd: 'one\\ntwo'});\r\n// \"quoted\"\t\\d+\n"
	for _, prefix := range []string{rawCustomPrefix, "codex2api.custom/"} {
		for depth := 0; depth <= 2; depth++ {
			_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			arguments := object{"summary": prefix + "functions.exec", "code": input}
			for i := 0; i < depth; i++ {
				encoded, _ := json.Marshal(arguments)
				envelope, _ := json.Marshal(object{"name": "functions.run_officejs", "arguments": string(encoded)})
				arguments = object{"summary": "Nested transport", "code": string(envelope)}
			}
			outer, _ := json.Marshal(arguments)
			native := nativeWeather()
			native["arguments"] = string(outer)
			converted, err := bridge.convertItem(native)
			if err != nil {
				t.Fatalf("prefix=%s depth=%d: %v", prefix, depth, err)
			}
			if converted["type"] != "custom_tool_call" || converted["name"] != "exec" || converted["input"] != input {
				t.Fatalf("custom input changed: %#v", converted)
			}
			if !reflect.DeepEqual(bridge.cache.get(bridge.scope+"/call_weather"), native) {
				t.Fatal("original nested replay item changed")
			}
		}
	}
}

func TestToolEnvelopeStillRejectsIncompleteOrMultipleCalls(t *testing.T) {
	call := `{"name":"functions.shell","arguments":{"cmd":"pwd"}}`
	for _, code := range []string{
		call + " " + call,
		"[" + call + "]",
		`{"name":"functions.shell","arguments":`,
		`{"name":"functions.shell","arguments":{"name":"functions.shell","arguments":{}}`,
		"functions.shell(" + call + "); functions.shell(" + call + ")",
		"functions.shell(" + call + ", " + call + ")",
		"unknown_tool(" + call + ")",
		"const first = " + call + "; const second = " + call + ";",
		"await tools.exec_command({cmd: 'pwd'});",
	} {
		_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := nativeWeather()
		outer, _ := json.Marshal(object{"code": code})
		native["arguments"] = string(outer)
		if _, err = bridge.convertItem(native); err == nil {
			t.Fatalf("ambiguous or incomplete input accepted: %s", code)
		}
	}
}

func TestToolEnvelopeErrorsIdentifyLayerWithoutArguments(t *testing.T) {
	const secret = "private-tool-contents"
	for _, stage := range []string{"outer_arguments", "transport_code", "nested_arguments", "function_arguments"} {
		_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := nativeWeather()
		bad := `{"private":"` + secret + `", "unfinished":`
		switch stage {
		case "outer_arguments":
			native["arguments"] = bad
		case "function_arguments":
			native["name"], native["arguments"] = "functions.shell", bad
		default:
			code := bad
			if stage == "nested_arguments" {
				encoded, _ := json.Marshal(object{"name": "run_officejs", "arguments": bad})
				code = string(encoded)
			}
			outer, _ := json.Marshal(object{"code": code})
			native["arguments"] = string(outer)
		}
		_, err = bridge.convertItem(native)
		if err == nil || !strings.Contains(err.Error(), "stage="+stage) || !strings.Contains(err.Error(), "json_offset=") || !strings.Contains(err.Error(), "unexpected_eof") || strings.Contains(err.Error(), secret) {
			t.Fatalf("stage=%s: missing or unsafe diagnostics: %v", stage, err)
		}
	}
}

func TestFormattedEnvelopeStreamsAndReplays(t *testing.T) {
	_, bridge, err := Prepare([]byte(testRequest), "scope", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	native := nativeWeather()
	outer, _ := json.Marshal(object{"code": "Tool request:\r\n" + strings.Repeat("\x60", 3) + "JSON\r\n" + `{"name":"weather","arguments":{"city":"Tokyo"}}` + "\r\n" + strings.Repeat("\x60", 3)})
	native["arguments"] = string(outer)
	event, _ := json.Marshal(object{"type": "response.completed", "response": object{"status": "completed", "output": []any{native}}})
	var output bytes.Buffer
	if err = bridge.Stream(strings.NewReader("data: "+string(event)+"\n\n"), func(raw []byte) error { output.Write(raw); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"name":"weather"`) || strings.Contains(output.String(), `"name":"run_officejs"`) {
		t.Fatalf("tool not converted: %s", output.String())
	}
}

func mustEnvelopeObject(t *testing.T, raw string) object {
	t.Helper()
	value, err := decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

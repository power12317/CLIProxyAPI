package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func functionTransportSource(name, field string) object {
	return object{"model": "gpt-6-astra", "input": []any{message("user", "inspect files")},
		"tools": []any{object{"type": "namespace", "name": "client", "tools": []any{object{
			"type": "function", "name": name, "parameters": object{"type": "object",
				"properties": object{field: object{"type": "string"}, "workdir": object{"type": "string"}}},
		}}}}}
}

func functionTransportNative(t *testing.T, name, field string, code, metadata any) object {
	t.Helper()
	outer, err := json.Marshal(object{"summary": "cpa.function_" + field + "/client." + name,
		"code": code, "extended_summary": metadata, "destructive": false, "references": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return object{"type": "function_call", "name": "run_officejs", "id": "native_raw",
		"call_id": "call_raw", "arguments": string(outer), "status": "completed",
		"encrypted_function_args": []any{"code"}}
}

func prepareFunctionTransport(t *testing.T, source object, scope string, cache *Cache) ([]byte, *Bridge) {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	wire, bridge, err := Prepare(raw, scope, "raw-session", cache)
	if err != nil {
		t.Fatal(err)
	}
	return wire, bridge
}

func TestFunctionRawTransportPreservesArgumentsAndHistory(t *testing.T) {
	for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
		for index, code := range []string{"", "  \tWrite-Output \"中文\"\r\n", "C:\\path\\file; literal \\n \\d+",
			"const x = {name: 'other_tool', args: {}};\n", strings.Repeat("quotes \"inside\"\n", 200)} {
			t.Run(fmt.Sprintf("%s/%d", field, index), func(t *testing.T) {
				source := functionTransportSource(name, field)
				cache := &Cache{}
				wire, bridge := prepareFunctionTransport(t, source, "account-a", cache)
				marker := "cpa.function_" + field + "/client." + name
				if !bytes.Contains(wire, []byte(marker)) {
					t.Fatal("raw function contract missing from catalog")
				}
				metadata := object{"workdir": "C:/workspace", "large": json.Number("9007199254740993"), "extra": object{"enabled": true}}
				meta, _ := json.Marshal(metadata)
				native := functionTransportNative(t, name, field, code, string(meta))
				before := clone(native)
				call, err := bridge.convertTool(native)
				if err != nil {
					t.Fatal(err)
				}
				want := clone(metadata)
				want[field] = code
				got, err := decode([]byte(stringValue(call["arguments"])))
				if err != nil || !reflect.DeepEqual(got, want) || call["namespace"] != "client" || call["name"] != name {
					t.Fatalf("function arguments or identity changed: %#v %v", call, err)
				}
				if !reflect.DeepEqual(call["encrypted_function_args"], []string{}) || !reflect.DeepEqual(native, before) {
					t.Fatal("encryption metadata or native item changed")
				}
				source["input"] = append(source["input"].([]any), call, object{"type": "function_call_output", "call_id": "call_raw", "output": "done"})
				for _, scope := range []string{"account-a", "account-b"} {
					replayed, next := prepareFunctionTransport(t, source, scope, cache)
					body, _ := decode(replayed)
					var restored object
					for _, item := range body["input"].([]any) {
						if candidate, ok := item.(object); ok && candidate["type"] == "function_call" {
							restored = candidate
						}
					}
					if scope == "account-a" && !reflect.DeepEqual(restored, before) {
						t.Fatal("cached native history changed")
					}
					outer, _ := decode([]byte(stringValue(restored["arguments"])))
					if outer["summary"] != marker || outer["code"] != code {
						t.Fatal("cache-miss history reintroduced nested source JSON")
					}
					roundtrip, err := next.convertTool(restored)
					if err != nil || roundtrip["arguments"] != call["arguments"] {
						t.Fatalf("history did not round trip: %v", err)
					}
				}
			})
		}
	}
}

func TestFunctionRawTransportRejectsMalformedEnvelope(t *testing.T) {
	for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
		source := functionTransportSource(name, field)
		_, bridge := prepareFunctionTransport(t, source, "account", &Cache{})
		for _, metadata := range []any{nil, 12, "", "[]", "null", "{} {}", "{invalid", fmt.Sprintf("{\"%s\":\"different\"}", field)} {
			_, err := bridge.convertTool(functionTransportNative(t, name, field, "private source", metadata))
			if err == nil || strings.Contains(err.Error(), "private source") {
				t.Fatalf("malformed metadata accepted or source leaked: %v", err)
			}
		}
		for _, value := range []any{nil, 12, object{}, []any{}} {
			if _, err := bridge.convertTool(functionTransportNative(t, name, field, value, "{}")); err == nil {
				t.Fatal("non-string source accepted")
			}
		}
		for _, target := range []string{"unknown", name + " "} {
			if _, err := bridge.convertTool(functionTransportNative(t, target, field, "source", "{}")); err == nil {
				t.Fatal("unknown tool accepted")
			}
		}
		source["tool_choice"] = "none"
		_, disabled := prepareFunctionTransport(t, source, "account", &Cache{})
		if _, err := disabled.convertTool(functionTransportNative(t, name, field, "source", "{}")); err == nil {
			t.Fatal("tool_choice none ignored")
		}
	}
}

func TestFunctionRawTransportStreamAndResponse(t *testing.T) {
	for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
		for _, invalid := range []bool{false, true} {
			source := functionTransportSource(name, field)
			metadata := "{\"workdir\":\"/tmp\"}"
			if invalid {
				metadata = "{invalid"
			}
			native := functionTransportNative(t, name, field, "printf \"hello\"\n", metadata)
			response := object{"id": "response_raw", "status": "completed", "output": []any{native}}
			raw, _ := json.Marshal(response)
			_, bridge := prepareFunctionTransport(t, source, "account", &Cache{})
			converted, err := bridge.Response(raw)
			if (err != nil) != invalid {
				t.Fatalf("non-stream response: %v", err)
			}
			_, bridge = prepareFunctionTransport(t, source, "account", &Cache{})
			event, _ := json.Marshal(object{"type": "response.completed", "response": response})
			var output bytes.Buffer
			err = bridge.Stream(strings.NewReader("data: "+string(event)+"\n\n"), func(p []byte) error { _, e := output.Write(p); return e })
			if (err != nil) != invalid {
				t.Fatalf("stream response: %v", err)
			}
			if invalid {
				if bytes.Contains(output.Bytes(), []byte("response.output_item.added")) {
					t.Fatal("invalid tool was dispatched")
				}
				continue
			}
			completed := CompletedResponse(output.Bytes()[strings.LastIndex(output.String(), "event: response.completed"):])
			if !bytes.Equal(completed, converted) {
				t.Fatalf("stream and non-stream differ: %s / %s", completed, converted)
			}
		}
	}
}

func TestFunctionRawTransportLegacyAndEncryptedHistory(t *testing.T) {
	for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
		for _, encrypted := range []bool{false, true} {
			source := functionTransportSource(name, field)
			cache := &Cache{}
			_, bridge := prepareFunctionTransport(t, source, "account", cache)
			args := object{field: "opaque-or-raw-source", "workdir": "/tmp"}
			encoded, _ := json.Marshal(args)
			legacy := functionTransportNative(t, name, field, "unused", "{}")
			if encrypted {
				legacy["name"] = "client." + name
				legacy["arguments"] = string(encoded)
				legacy["encrypted_function_args"] = []any{field}
			} else {
				envelope, _ := json.Marshal(object{"name": "client." + name, "arguments": args})
				outer, _ := json.Marshal(object{"summary": "Legacy function", "code": string(envelope)})
				legacy["arguments"] = string(outer)
			}
			call, err := bridge.convertTool(legacy)
			if err != nil || call["arguments"] != string(encoded) {
				t.Fatalf("legacy function conversion changed: %v", err)
			}
			if encrypted && !reflect.DeepEqual(call["encrypted_function_args"], []any{field}) {
				t.Fatal("direct encrypted argument metadata changed")
			}
			if !encrypted && !reflect.DeepEqual(cache.get(bridge.scope+"/call_raw"), legacy) {
				t.Fatal("cached legacy wrapper changed")
			}
			// Unavailable historical tools and encrypted direct calls use the old envelope.
			if !encrypted {
				delete(bridge.tools, "client."+name)
			}
			rebuilt, err := bridge.rebuildToolCall(call)
			if err != nil {
				t.Fatal(err)
			}
			outer, _ := decode([]byte(stringValue(rebuilt["arguments"])))
			if strings.HasPrefix(stringValue(outer["summary"]), rawFunctionPrefix) {
				t.Fatal("historical fallback changed")
			}
			envelope, err := decode([]byte(stringValue(outer["code"])))
			if err != nil || !reflect.DeepEqual(envelope["arguments"], args) {
				t.Fatal("history arguments changed")
			}
		}
	}
}

func TestFunctionRawTransportContractAndDuplicates(t *testing.T) {
	for _, tc := range []struct{ name, kind, codeType, cmdType, want string }{
		{"run_code", "function", "string", "", "code"},
		{"exec_command", "function", "", "string", "cmd"},
		{"exec_command", "function", "string", "string", "code"},
		{"exec_command", "custom", "string", "string", ""},
		{"other", "function", "", "string", ""},
		{"run_code", "function", "number", "", ""},
	} {
		spec := tool{Name: tc.name, Namespace: "client", Type: tc.kind, Parameters: object{"type": "object", "properties": object{"code": object{"type": tc.codeType}, "cmd": object{"type": tc.cmdType}}}}
		if got := rawFunctionField(spec); got != tc.want {
			t.Fatalf("contract %v: field=%q", tc, got)
		}
	}
	for field, name := range map[string]string{"code": "run_code", "cmd": "exec_command"} {
		_, bridge := prepareFunctionTransport(t, functionTransportSource(name, field), "account", &Cache{})
		metadata, _ := json.Marshal(object{field: "identical source", "extra": true})
		call, err := bridge.convertTool(functionTransportNative(t, name, field, "identical source", string(metadata)))
		if err != nil || call["arguments"] != string(metadata) {
			t.Fatalf("identical duplicate changed: %v", err)
		}
		otherField := "cmd"
		if field == "cmd" {
			otherField = "code"
		}
		if _, err := bridge.convertTool(functionTransportNative(t, name, otherField, "source", "{}")); err == nil {
			t.Fatal("wrong source marker accepted")
		}
	}
}

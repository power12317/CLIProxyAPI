package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReplayFingerprintMatchesBusinessArguments(t *testing.T) {
	cache := &Cache{}
	_, bridge, err := Prepare([]byte(testRequest), "account", "session", cache)
	if err != nil {
		t.Fatal(err)
	}
	native := nativeWeather()
	call, err := bridge.convertTool(native)
	if err != nil {
		t.Fatal(err)
	}
	key := bridge.scope + "/call_weather"
	same := clone(call)
	same["id"] = "different-item"
	same["status"] = "in_progress"
	same["arguments"] = "{\n \"city\" : \"Tokyo\" }"
	delete(same, "encrypted_function_args")
	if got := cache.getForCall(key, same); digest(got) != digest(native) {
		t.Fatal("equivalent call did not retain native replay")
	}
	for _, change := range []func(object){
		func(call object) { call["arguments"] = `{"city":"Osaka"}` },
		func(call object) { call["name"] = "different" },
		func(call object) { call["namespace"] = "other" },
		func(call object) { call["encrypted_function_args"] = []any{"city"} },
		func(call object) { call["arguments"] = `{"broken":` },
	} {
		changed := clone(call)
		change(changed)
		if cache.getForCall(key, changed) != nil {
			t.Fatal("changed call matched cached native item")
		}
	}
	changed := clone(call)
	changed["arguments"] = `{"city":"Osaka"}`
	source := object{"model": "test-model", "tools": []any{object{"type": "function", "name": "weather"}}, "input": []any{changed, object{"type": "function_call_output", "call_id": "call_weather", "output": "current result"}}}
	raw, _ := json.Marshal(source)
	wire, _, err := Prepare(raw, "account", "session", cache)
	if err != nil || strings.Contains(string(wire), "Tokyo") || !strings.Contains(string(wire), "Osaka") {
		t.Fatalf("stale cache won over current history: %s %v", wire, err)
	}
}

func TestReplayFingerprintPreservesCustomInputAndClientSearch(t *testing.T) {
	for _, call := range []object{
		{"type": "custom_tool_call", "name": "exec", "namespace": "functions", "call_id": "c", "input": "exact\r\nsource\\"},
		{"type": "tool_search_call", "execution": "client", "call_id": "s", "arguments": object{"query": "edit"}},
	} {
		cache := &Cache{}
		native := nativeWeather()
		if err := cache.put("scope", native, call); err != nil {
			t.Fatal(err)
		}
		if cache.getForCall("scope", call) == nil {
			t.Fatal("exact call did not match")
		}
		changed := clone(call)
		if call["type"] == "custom_tool_call" {
			changed["input"] = "exact\nsource\\"
		} else {
			changed["arguments"] = object{"query": "read"}
		}
		if cache.getForCall("scope", changed) != nil {
			t.Fatal("changed raw input/search arguments matched")
		}
	}
}

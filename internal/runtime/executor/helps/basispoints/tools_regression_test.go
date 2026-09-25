package basispoints

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func catalogRequest() []byte {
	return []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"inspect and patch"},{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"Execute exact JavaScript","format":{"type":"grammar","definition":"start: CODE"}}]}]}],"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"shell","description":"Inspect files","inputSchema":{"type":"object","required":["cmd"],"additionalProperties":false,"properties":{"cmd":{"type":"string"}}},"strict":true}]}]}`)
}

func TestAdditionalToolsAndCustomContractsAreAvailable(t *testing.T) {
	raw := catalogRequest()
	wire, bridge, err := Prepare(raw, "account-a/caller", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bridge.tools) != 2 || gjson.GetBytes(wire, "input.#").Int() != 2 || bytes.Contains(wire, []byte(`"type":"additional_tools"`)) {
		t.Fatalf("additional tools were not translated into the catalog: %s", wire)
	}
	catalog := gjson.GetBytes(wire, "input.0.content.0.text").String()
	for _, value := range []string{"functions.shell", "functions.exec", "Execute exact JavaScript", "start: CODE", `"required":["cmd"]`, `"additionalProperties":false`, "cpa.custom/functions.exec"} {
		if !strings.Contains(catalog, value) {
			t.Errorf("lost tool contract %q: %s", value, catalog)
		}
	}
	for i := 0; i < 20; i++ {
		retry, _, errRetry := Prepare(raw, "account-a/caller", "session", &Cache{})
		if errRetry != nil || !bytes.Equal(wire, retry) {
			t.Fatal("tool catalog changed between identical requests")
		}
	}
}

func TestRawCustomAndDirectFunctionCallsRoundTripAcrossAccounts(t *testing.T) {
	for _, custom := range []bool{false, true} {
		raw := catalogRequest()
		cache := &Cache{}
		_, bridge, err := Prepare(raw, "account-a/caller", "session", cache)
		if err != nil {
			t.Fatal(err)
		}
		native := object{"id": "native-complete-item", "type": "function_call", "call_id": "call_test", "name": "functions.shell", "arguments": `{"cmd":"pwd","large":9007199254740993}`}
		const exactInput = "const x = 'quoted \\\\ value';\nawait tool({nested: {value: 42}});\n"
		if custom {
			args, _ := json.Marshal(object{"summary": "cpa.custom/functions.exec", "code": exactInput})
			native["name"], native["arguments"] = "run_officejs", string(args)
		}
		client, err := bridge.convertItem(native)
		if err != nil {
			t.Fatal(err)
		}
		if client["namespace"] != "functions" || client["call_id"] != "call_test" {
			t.Fatalf("client tool identity changed: %#v", client)
		}
		if custom && client["input"] != exactInput {
			t.Fatal("raw custom input was altered")
		}
		if !custom && !strings.Contains(stringValue(client["arguments"]), "9007199254740993") {
			t.Fatal("function argument precision was lost")
		}
		body, _ := decode(raw)
		outputType := "function_call_output"
		if custom {
			outputType = "custom_tool_call_output"
		}
		body["input"] = append(body["input"].([]any), client, object{"id": "ctco_result", "type": outputType, "call_id": "call_test", "output": "exact tool result"})
		replay, _ := json.Marshal(body)
		for _, scope := range []string{"account-a/caller", "account-b/caller"} {
			wire, _, errReplay := Prepare(replay, scope, "session", cache)
			if errReplay != nil {
				t.Fatalf("account rotation must remain allowed: %v", errReplay)
			}
			call, result := gjson.GetBytes(wire, "input.2"), gjson.GetBytes(wire, "input.3")
			if call.Get("name").String() != "run_officejs" || result.Get("type").String() != "function_call_output" || result.Get("output").String() != "exact tool result" || call.Get("id").String() == result.Get("id").String() {
				t.Fatalf("invalid tool history: %s", wire)
			}
		}
	}
}

func TestNativePlanIsAdaptedToDeclaredClientTool(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","input":"plan","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"update_plan","parameters":{"type":"object","properties":{"plan":{"type":"array"}}}}]}]}`)
	_, bridge, err := Prepare(raw, "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"update_plan", "functions.update_plan"} {
		call, err := bridge.convertItem(object{"type": "function_call", "name": name, "call_id": "plan-call", "arguments": `{"summary":"Update","plan":[{"description":"Inspect files","status":"done"}]}`})
		if err != nil {
			t.Fatal(err)
		}
		args := gjson.Parse(stringValue(call["arguments"]))
		if call["name"] != "update_plan" || call["namespace"] != "functions" || args.Get("plan.0.step").String() != "Inspect files" || args.Get("plan.0.status").String() != "completed" || args.Get("explanation").String() != "Update" {
			t.Fatalf("plan tool was lost or malformed: %#v", call)
		}
	}
}

func TestToolStreamUsesCompleteTerminalItem(t *testing.T) {
	cache := &Cache{}
	_, bridge, err := Prepare([]byte(testRequest), "scope", "session", cache)
	if err != nil {
		t.Fatal(err)
	}
	partial := nativeWeather()
	partial["arguments"] = `{"summary":"partial","code":""}`
	var wire bytes.Buffer
	for _, event := range []object{
		{"type": "response.output_text.delta", "delta": "Checking"},
		{"type": "response.output_item.done", "output_index": 0, "item": partial},
		{"type": "response.completed", "response": object{"status": "completed", "output": []any{nativeWeather()}}},
	} {
		raw, _ := json.Marshal(event)
		wire.WriteString("data: " + string(raw) + "\n\n")
	}
	var output bytes.Buffer
	err = bridge.Stream(&wire, func(event []byte) error { output.Write(event); return nil })
	if err != nil || !strings.Contains(output.String(), `"name":"weather"`) || !strings.Contains(output.String(), "Checking") {
		t.Fatalf("terminal tool conversion failed: %v %s", err, output.String())
	}
	if cached := cache.get(bridge.scope + "/call_weather"); cached["arguments"] != nativeWeather()["arguments"] {
		t.Fatal("partial native arguments replaced the complete server item")
	}
}

package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func clientSearchSource() object {
	return object{"model": "gpt-6-astra", "input": []any{message("user", "edit a local spreadsheet")}, "tools": []any{
		object{"type": "tool_search", "execution": "client", "description": "Find local tools", "parameters": object{"type": "object", "properties": object{"query": object{"type": "string"}}}},
		object{"type": "web_search", "external_web_access": true},
	}}
}

func searchNativeCall() object {
	envelope, _ := json.Marshal(object{"name": "tool_search", "arguments": object{"query": "spreadsheet editor"}})
	outer, _ := json.Marshal(object{"summary": "Find local tools", "code": string(envelope)})
	return object{"type": "function_call", "name": "run_officejs", "id": "search-item", "call_id": "search-call", "arguments": string(outer), "status": "completed"}
}

func TestClientToolSearchDiscoveryAndReplay(t *testing.T) {
	source := clientSearchSource()
	cache := &Cache{}
	_, bridge := prepareFunctionTransport(t, source, "account-a", cache)
	native := searchNativeCall()
	call, err := bridge.convertItem(native)
	if err != nil {
		t.Fatal(err)
	}
	if call["type"] != "tool_search_call" || call["execution"] != "client" || call["call_id"] != "search-call" || call["name"] != nil || !reflect.DeepEqual(call["arguments"], object{"query": "spreadsheet editor"}) {
		t.Fatalf("wrong client search call: %#v", call)
	}
	loaded := []any{object{"type": "namespace", "name": "local", "tools": []any{object{"type": "function", "name": "edit_sheet", "description": "Edit spreadsheet", "parameters": object{"type": "object"}}}}}
	searchOutput := object{"type": "tool_search_output", "execution": "client", "call_id": "search-call", "status": "completed", "tools": loaded}
	source["input"] = append(source["input"].([]any), call, searchOutput)
	for _, scope := range []string{"account-a", "account-b"} {
		wire, next := prepareFunctionTransport(t, source, scope, cache)
		if _, exists := next.tools["local.edit_sheet"]; !exists {
			t.Fatal("discovered tool missing from callable catalog")
		}
		if gjson.GetBytes(wire, "input.2.type").String() != "function_call" || gjson.GetBytes(wire, "input.3.type").String() != "function_call_output" {
			t.Fatalf("discovery history was not relayed: %s", wire)
		}
		output := gjson.Parse(gjson.GetBytes(wire, "input.3.output").String())
		if output.Get("tools.0.name").String() != "local" || output.Get("call_id").String() != "search-call" {
			t.Fatal("discovery output lost original tools or identity")
		}
		if scope == "account-a" {
			got, _ := decode([]byte(gjson.GetBytes(wire, "input.2").Raw))
			if !reflect.DeepEqual(got, native) {
				t.Fatal("cached native search item changed")
			}
		}
		envelope, _ := json.Marshal(object{"name": "local.edit_sheet", "arguments": object{}})
		outer, _ := json.Marshal(object{"code": string(envelope)})
		item := nativeWeather()
		item["arguments"] = string(outer)
		converted, err := next.convertItem(item)
		if err != nil || converted["name"] != "edit_sheet" || converted["namespace"] != "local" {
			t.Fatalf("loaded tool is not callable: %v", err)
		}
	}
}

func TestClientToolSearchStreamingEvents(t *testing.T) {
	_, bridge := prepareFunctionTransport(t, clientSearchSource(), "account", &Cache{})
	response := object{"id": "search-response", "status": "completed", "output": []any{searchNativeCall()}}
	event, _ := json.Marshal(object{"type": "response.completed", "response": response})
	var out bytes.Buffer
	err := bridge.Stream(strings.NewReader("data: "+string(event)+"\n\n"), func(raw []byte) error { _, errWrite := out.Write(raw); return errWrite })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "response.function_call_arguments") || strings.Contains(out.String(), "run_officejs") {
		t.Fatal("function transport leaked into client discovery events")
	}
	if strings.Count(out.String(), "event: response.output_item.done") != 1 {
		t.Fatal("client discovery was not emitted exactly once")
	}
	final := CompletedResponse(out.Bytes()[strings.LastIndex(out.String(), "event: response.completed"):])
	if gjson.GetBytes(final, "output.0.arguments.query").String() != "spreadsheet editor" || gjson.GetBytes(final, "output.0.execution").String() != "client" {
		t.Fatalf("client discovery shape changed: %s", final)
	}
}

func TestNativeCapabilityDeclarationsAndSelection(t *testing.T) {
	source := clientSearchSource()
	wire, bridge := prepareFunctionTransport(t, source, "account", &Cache{})
	if !bytes.Contains(wire, []byte("cpa.native/web_search")) || !bytes.Contains(wire, []byte("external_web_access")) {
		t.Fatal("optional native capability was removed")
	}
	outer, _ := json.Marshal(object{"summary": "cpa.native/web_search", "code": "", "extended_summary": "Need live web results"})
	intent := nativeWeather()
	intent["arguments"] = string(outer)
	response := object{"status": "completed", "output": []any{searchNativeCall(), intent}}
	raw, _ := json.Marshal(response)
	_, err := bridge.Response(raw)
	if RequestedNativeTool(err) != "web_search" {
		t.Fatalf("native tool intent not recognized: %v", err)
	}
	if bridge.cache.get(bridge.scope+"/search-call") != nil {
		t.Fatal("mixed native batch committed a speculative client tool")
	}
	for _, choice := range []any{"none", object{"type": "tool_search"}} {
		source["tool_choice"] = choice
		_, disabled := prepareFunctionTransport(t, source, "account", &Cache{})
		_, err := disabled.convertItem(intent)
		if err == nil || RequestedNativeTool(err) != "" {
			t.Fatal("native switch ignored explicit client selection")
		}
	}
}

func TestNativeCapabilityPromptDoesNotExposeAuthentication(t *testing.T) {
	source := clientSearchSource()
	source["tools"] = append(source["tools"].([]any), object{"type": "mcp", "server_label": "workspace", "authorization": "private-authorization", "headers": object{"X-Key": "private-header"}})
	wire, bridge := prepareFunctionTransport(t, source, "account", &Cache{})
	if bytes.Contains(wire, []byte("private-authorization")) || bytes.Contains(wire, []byte("private-header")) {
		t.Fatal("native authentication was placed in the model prompt")
	}
	if bridge.nativeTools["mcp"][0]["authorization"] != "private-authorization" {
		t.Fatal("original native declaration was modified")
	}
}

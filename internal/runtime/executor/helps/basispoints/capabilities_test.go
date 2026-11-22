package basispoints

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

func TestUnmappedNativeToolFamiliesRequestOneChannelSwitch(t *testing.T) {
	names := strings.Fields("update_plan request_user_input_basispoints read_ranges list_skills read_skills create_skill update_skill search_workbook list_items list_connectors read_sheets_metadata run_connector_action format_range write_range clear_range update_sheet update_workbook update_sheet_view copy_range_to read_range_image resize_range pivot_table chart table future_provider_tool")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, bridge, err := Prepare(catalogRequest(), "caller/account", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			native := object{"type": "function_call", "name": name, "call_id": "unmapped-call", "id": "unmapped-item", "arguments": "{}"}
			response := object{"status": "completed", "output": []any{native}}
			raw, _ := json.Marshal(response)
			if _, err := bridge.Response(raw); RequestedNativeTool(err) != "unmapped_tool/"+name {
				t.Fatalf("unsupported provider handler was not routed: %v", err)
			}
			if bridge.cache.get(bridge.scope+"/unmapped-call") != nil {
				t.Fatal("a rejected native batch committed replay state")
			}
		})
	}
}

func TestNativeToolsUseOnlyUnambiguousDeclaredHandlers(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		source := object{"model": "fixture", "input": "read skills", "tools": []any{
			object{"type": "namespace", "name": "skills", "tools": []any{object{
				"type": "function", "name": "list_skills", "parameters": object{
					"type": "object", "required": []any{"limit"}, "additionalProperties": false,
					"properties": object{"limit": object{"type": "integer", "minimum": 1}},
				},
			}}},
		}}
		if ambiguous {
			source["tools"] = append(source["tools"].([]any), object{"type": "namespace", "name": "other", "tools": []any{object{"type": "function", "name": "list_skills"}}})
		}
		raw, _ := json.Marshal(source)
		_, bridge, err := Prepare(raw, "account-a", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := object{"type": "function_call", "name": "list_skills", "call_id": "skills-call", "arguments": `{"limit":8}`}
		response, _ := json.Marshal(object{"status": "completed", "output": []any{native}})
		converted, err := bridge.Response(response)
		if ambiguous {
			if RequestedNativeTool(err) == "" {
				t.Fatal("ambiguous client namespaces were guessed")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		call := ojson.GetBytes(converted, "output.0")
		if call.Get("namespace").String() != "skills" || call.Get("name").String() != "list_skills" || call.Get("arguments").String() != `{"limit":8}` {
			t.Fatal("declared handler identity or arguments changed")
		}
		client, _ := decode([]byte(call.Raw))
		source["input"] = []any{message("user", "read skills"), client, object{"type": "function_call_output", "call_id": "skills-call", "output": "actual skills"}}
		history, _ := json.Marshal(source)
		for _, scope := range []string{"account-a", "account-b"} {
			wire, next, err := Prepare(history, scope, "session", bridge.cache)
			if err != nil {
				t.Fatal(err)
			}
			items := ojson.GetBytes(wire, "input").Array()
			var replay object
			for _, item := range items {
				if item.Get("type").String() == "function_call" {
					replay, _ = decode([]byte(item.Raw))
				}
			}
			got, err := next.convertTool(replay)
			if err != nil || stringValue(got["name"]) != "list_skills" || stringValue(got["arguments"]) != `{"limit":8}` {
				t.Fatalf("real tool result did not replay across accounts: %v", err)
			}
		}
		native["arguments"] = `{"limit":"wrong type"}`
		response, _ = json.Marshal(object{"status": "completed", "output": []any{native}})
		if _, err := bridge.Response(response); RequestedNativeTool(err) != "incompatible_tool/list_skills" {
			t.Fatalf("incompatible direct native arguments were dispatched: %v", err)
		}
	}
}

func TestResponseCatalogContainsOnlyClientExecutionContracts(t *testing.T) {
	_, bridge, err := Prepare(catalogRequest(), "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	response := object{"id": "catalog", "status": "completed", "tools": []any{
		object{"type": "function", "name": "list_skills"}, object{"type": "function", "name": "run_officejs"},
	}, "output": []any{}}
	raw, _ := json.Marshal(response)
	converted, err := bridge.Response(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(converted, []byte("list_skills")) || bytes.Contains(converted, []byte("run_officejs")) || ojson.GetBytes(converted, "tools.0.name").String() != "functions" {
		t.Fatalf("response advertised provider handlers as client tools: %s", converted)
	}
	event, _ := json.Marshal(object{"type": "response.completed", "response": response})
	var output bytes.Buffer
	if err := bridge.Stream(strings.NewReader("data: "+string(event)+"\n\n"), func(frame []byte) error { output.Write(frame); return nil }); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("list_skills")) || bytes.Contains(output.Bytes(), []byte("run_officejs")) {
		t.Fatal("stream metadata exposed provider-only tools")
	}
}

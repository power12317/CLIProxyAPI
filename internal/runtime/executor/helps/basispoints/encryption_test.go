package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFunctionEncryptionMetadataAcrossResponseModes(t *testing.T) {
	for _, toolName := range []string{"spawn_agent", "send_message", "followup_task"} {
		for _, mode := range []struct{ direct, encrypted bool }{{false, false}, {true, false}, {true, true}} {
			direct := mode.direct
			for _, stream := range []bool{false, true} {
				request := object{"model": "gpt-6-astra", "input": "Continue the task", "tools": []any{
					object{"type": "namespace", "name": "collaboration", "tools": []any{
						object{"type": "function", "name": toolName, "parameters": object{"type": "object", "properties": object{
							"message": object{"type": "string", "encrypted": true},
						}}},
					}},
				}}
				rawRequest, _ := json.Marshal(request)
				cache := &Cache{}
				_, bridge, err := Prepare(rawRequest, "account-a", "session", cache)
				if err != nil {
					t.Fatal(err)
				}
				args := object{"message": "Exact task: 中文, \"quotes\", \\ paths.\r\n", "target": "worker"}
				wantMetadata := []any{}
				if mode.encrypted {
					args["message"] = "opaque-ciphertext"
					wantMetadata = []any{"message"}
				}
				encodedArgs, _ := json.Marshal(args)
				envelope, _ := json.Marshal(object{"name": "collaboration." + toolName, "arguments": args})
				transport, _ := json.Marshal(object{"summary": "Relay task", "code": string(envelope)})
				native := object{"type": "function_call", "id": "native-call", "call_id": "call-task", "name": "run_officejs", "arguments": string(transport), "encrypted_function_args": []any{"code"}}
				if direct {
					native["name"], native["arguments"] = "collaboration."+toolName, string(encodedArgs)
					delete(native, "encrypted_function_args")
					if mode.encrypted {
						native["encrypted_function_args"] = wantMetadata
					}
				}
				original := clone(native)
				var completed object
				if stream {
					upstream, _ := json.Marshal(object{"type": "response.completed", "response": object{"status": "completed", "output": []any{native}}})
					markedItems := 0
					err = bridge.Stream(strings.NewReader("data: "+string(upstream)+"\n\n"), func(chunk []byte) error {
						for _, line := range bytes.Split(chunk, []byte("\n")) {
							if !bytes.HasPrefix(line, []byte("data: ")) {
								continue
							}
							event, errEvent := decode(bytes.TrimPrefix(line, []byte("data: ")))
							if errEvent != nil {
								return errEvent
							}
							if item, ok := event["item"].(object); ok {
								assertEncryptionMetadata(t, item, wantMetadata)
								markedItems++
							}
							if event["type"] == "response.completed" {
								completed, _ = event["response"].(object)
							}
						}
						return nil
					})
					if markedItems != 2 {
						t.Fatalf("encryption metadata not present in both item events: %d", markedItems)
					}
				} else {
					upstream, _ := json.Marshal(object{"status": "completed", "output": []any{native}})
					var converted []byte
					converted, err = bridge.Response(upstream)
					if err == nil {
						completed, err = decode(converted)
					}
				}
				if err != nil {
					t.Fatalf("%s/direct=%v/stream=%v: %v", toolName, direct, stream, err)
				}
				call := completed["output"].([]any)[0].(object)
				assertEncryptionMetadata(t, call, wantMetadata)
				if call["name"] != toolName || call["namespace"] != "collaboration" || call["arguments"] != string(encodedArgs) {
					t.Fatal("client tool identity or task text changed")
				}
				if !reflect.DeepEqual(native, original) {
					t.Fatal("native item mutated")
				}
				if !direct && !reflect.DeepEqual(cache.get(bridge.scope+"/call-task"), original) {
					t.Fatal("wrapper encryption metadata lost from native replay")
				}
				request["input"] = []any{call, object{"type": "function_call_output", "call_id": "call-task", "output": "delivered"}}
				replay, _ := json.Marshal(request)
				for _, account := range []string{"account-a", "account-b"} {
					if _, _, errReplay := Prepare(replay, account, "session", cache); errReplay != nil {
						t.Fatalf("account rotation rejected: %v", errReplay)
					}
				}
			}
		}
	}
}

func TestDirectFunctionPreservesDeclaredEncryption(t *testing.T) {
	for _, metadata := range []any{nil, []any{}, []any{"city"}} {
		_, bridge, err := Prepare([]byte(testRequest), "account", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		native := object{"type": "function_call", "name": "weather", "call_id": "direct", "arguments": `{"city":"opaque-encrypted-value"}`, "encrypted_function_args": metadata}
		call, err := bridge.convertTool(native)
		if err != nil {
			t.Fatal(err)
		}
		want := metadata
		if want == nil {
			want = []any{}
		}
		assertEncryptionMetadata(t, call, want)
		if call["arguments"] != native["arguments"] {
			t.Fatal("direct ciphertext changed")
		}
	}
}

func TestAgentMessagesKeepTheirOriginalContent(t *testing.T) {
	for _, kind := range []string{"input_text", "encrypted_content"} {
		field := "text"
		if kind == "encrypted_content" {
			field = "encrypted_content"
		}
		message := object{"type": "agent_message", "author": "/root", "recipient": "/root/worker", "content": []any{
			object{"type": kind, field: "original message or opaque ciphertext"},
		}}
		request, _ := json.Marshal(object{"model": "gpt-6-astra", "input": []any{message}})
		wire, _, err := Prepare(request, "different-account", "child-session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := decode(wire)
		if err != nil || !reflect.DeepEqual(body["input"].([]any)[1], message) {
			t.Fatal("agent message content was changed or dropped")
		}
	}
}

func TestEncryptedPlanArgumentsAreNotRenamed(t *testing.T) {
	request := []byte(`{"model":"gpt-6-astra","input":"plan","tools":[{"type":"function","name":"update_plan","parameters":{"type":"object","properties":{"plan":{"type":"array"}}}}]}`)
	_, bridge, err := Prepare(request, "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	native := object{"type": "function_call", "name": "update_plan", "call_id": "plan",
		"arguments":               `{"summary":"opaque-summary","plan":[{"description":"opaque-step","status":"done"}]}`,
		"encrypted_function_args": []any{"summary", "plan"},
	}
	call, err := bridge.convertTool(native)
	if err != nil {
		t.Fatal(err)
	}
	assertEncryptionMetadata(t, call, native["encrypted_function_args"])
	original, _ := decode([]byte(stringValue(native["arguments"])))
	converted, _ := decode([]byte(stringValue(call["arguments"])))
	if !reflect.DeepEqual(converted, original) {
		t.Fatal("encrypted plan arguments were reinterpreted")
	}
}

func assertEncryptionMetadata(t *testing.T, item object, expected any) {
	t.Helper()
	actual, present := item["encrypted_function_args"]
	got, _ := json.Marshal(actual)
	want, _ := json.Marshal(expected)
	if !present || !bytes.Equal(got, want) {
		t.Fatalf("encrypted_function_args: present=%v got=%s want=%s", present, got, want)
	}
}

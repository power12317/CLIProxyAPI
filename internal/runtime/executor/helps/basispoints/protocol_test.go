package basispoints

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const testRequest = `{"model":"gpt-6-astra","input":[{"role":"user","content":"weather"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`

func nativeWeather() object {
	return object{"id": "native_item", "call_id": "call_weather", "type": "function_call", "name": "run_officejs", "status": "completed", "arguments": `{"summary":"Weather","extended_summary":"Fetch weather","code":"{\"tool\":\"weather\",\"args\":{\"city\":\"Tokyo\"}}","references":["Tokyo"],"destructive":false}`}
}

func TestToolRoundTripKeepsFullItemAndTurn(t *testing.T) {
	cache := &Cache{}
	body, bridge, err := Prepare([]byte(testRequest), "account/caller", "session", cache)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "tools").Exists() {
		t.Fatal("client tools leaked upstream")
	}
	if !strings.Contains(string(body), "weather") {
		t.Fatal("missing tool catalog")
	}
	response := object{"model": "gpt-6-astra", "status": "completed", "output": []any{nativeWeather()}}
	bridge.BindCredential("caller", "account")
	raw, _ := json.Marshal(response)
	converted, err := bridge.Response(raw)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(converted, "output.0.name").String() != "weather" || gjson.GetBytes(converted, "output.0.arguments").String() != `{"city":"Tokyo"}` {
		t.Fatalf("bad converted tool: %s", converted)
	}
	var client object
	_ = json.Unmarshal([]byte(testRequest), &client)
	var result object
	_ = json.Unmarshal(converted, &result)
	client["input"] = append(client["input"].([]any), result["output"].([]any)[0], object{"type": "function_call_output", "call_id": "call_weather", "output": "18C"})
	replay, _ := json.Marshal(client)
	if cache.Owner("caller", replay) != "account" || cache.Owner("other-caller", replay) != "" {
		t.Fatal("tool credential ownership is not isolated")
	}
	second, _, err := Prepare(replay, "account/caller", "session", cache)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(second, "input.2"); got.Get("id").String() != "native_item" || got.Get("arguments").String() != nativeWeather()["arguments"] {
		t.Fatalf("native item not restored: %s", second)
	}
	if gjson.GetBytes(body, "metadata.turn_id").String() != gjson.GetBytes(second, "metadata.turn_id").String() {
		t.Fatal("tool result changed turn")
	}
	if gjson.GetBytes(second, "metadata.agent_iteration").String() != "2" {
		t.Fatal("iteration did not advance")
	}
	retry, _, err := Prepare(replay, "account/caller", "session", cache)
	if err != nil || !bytes.Equal(second, retry) {
		t.Fatal("retry changed request identity")
	}
	client["input"] = append(client["input"].([]any), object{"role": "user", "content": "weather"})
	next, _ := json.Marshal(client)
	third, _, err := Prepare(next, "account/caller", "session", cache)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(third, "metadata.turn_id").String() == gjson.GetBytes(second, "metadata.turn_id").String() {
		t.Fatal("identical new user text reused the old turn")
	}
	if _, _, err = Prepare(replay, "other-account", "session", cache); err == nil {
		t.Fatal("cross-account replay accepted")
	}
}

func TestStreamForwardsTextBeforeTerminalAndConvertsTools(t *testing.T) {
	_, bridge, err := Prepare([]byte(testRequest), "stream-account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	first := make(chan struct{})
	done := make(chan error, 1)
	var received bytes.Buffer
	go func() {
		done <- bridge.Stream(reader, func(event []byte) error {
			received.Write(event)
			if bytes.Contains(event, []byte("response.output_text.delta")) {
				close(first)
			}
			return nil
		})
	}()
	_, err = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"checking\"}\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-t.Context().Done():
		t.Fatal("text was buffered until completion")
	}
	added := nativeWeather()
	added["arguments"] = ""
	addedRaw, _ := json.Marshal(object{"type": "response.output_item.added", "output_index": 1, "item": added})
	doneRaw, _ := json.Marshal(object{"type": "response.output_item.done", "output_index": 1, "item": nativeWeather()})
	terminalRaw, _ := json.Marshal(object{"type": "response.completed", "response": object{"output": []any{object{"type": "message"}, nativeWeather()}, "status": "completed"}})
	for _, raw := range [][]byte{addedRaw, doneRaw, terminalRaw} {
		if _, err = writer.Write(append(append([]byte("data: "), raw...), '\n', '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(received.String(), "run_officejs") {
		t.Fatal("native tool leaked to client")
	}
	if strings.Count(received.String(), "event: response.output_item.added") != 1 {
		t.Fatal("tool item emitted more than once")
	}
	completed := CompletedResponse([]byte(received.String()[strings.LastIndex(received.String(), "event: response.completed"):]))
	if gjson.GetBytes(completed, "output.1.name").String() != "weather" {
		t.Fatalf("terminal output differs: %s", completed)
	}
}

func TestStreamRejectsTruncatedAndMalformedToolOutput(t *testing.T) {
	_, bridge, err := Prepare([]byte(testRequest), "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	if err = bridge.Stream(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"), func([]byte) error { return nil }); err == nil {
		t.Fatal("truncated stream accepted")
	}
	item := nativeWeather()
	item["arguments"] = `{"code":"alert(1)"}`
	if _, err = bridge.convertItem(item); err == nil {
		t.Fatal("JavaScript accepted")
	}
	item["arguments"] = `{"code":"{\"tool\":\"unknown\",\"args\":{}}"}`
	if _, err = bridge.convertItem(item); err == nil {
		t.Fatal("undeclared tool accepted")
	}
}

func TestNamespaceCustomTools(t *testing.T) {
	request := `{"model":"arbitrary-model","input":"edit","tools":[{"type":"namespace","name":"files","tools":[{"type":"custom","name":"patch"}]}]}`
	_, bridge, err := Prepare([]byte(request), "account", "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	item := nativeWeather()
	item["arguments"] = `{"code":"{\"tool\":\"files.patch\",\"args\":\"*** Begin Patch\\n*** End Patch\"}"}`
	converted, err := bridge.convertItem(item)
	if err != nil {
		t.Fatal(err)
	}
	if converted["type"] != "custom_tool_call" || converted["namespace"] != "files" || converted["name"] != "patch" {
		t.Fatalf("bad custom call: %#v", converted)
	}
	if converted["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input changed: %#v", converted)
	}
	for _, effort := range []string{"low", "max", "ultra"} {
		raw, _ := sjson.SetBytes([]byte(request), "reasoning_effort", effort)
		body, _, errPrepare := Prepare(raw, "account", "session", &Cache{})
		if errPrepare != nil || gjson.GetBytes(body, "reasoning_effort").String() != effort {
			t.Fatalf("effort changed: %s %v", body, errPrepare)
		}
	}
}

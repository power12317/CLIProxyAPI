package helps

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"testing"
)

func TestCodexContinuationRequiresFullPrefixAndMatchingProperties(t *testing.T) {
	state := &CodexContinuation{}
	request := []byte(`{"model":"gpt-5.4","instructions":"system","tools":[],"input":[{"role":"user","content":"hello"}],"client_metadata":{"turn_id":"one"}}`)
	response := []byte(`{"type":"response.completed","response":{"id":"resp-one","output":[{"type":"function_call","call_id":"call-one","name":"lookup","arguments":"{}"}]}}`)
	state.Complete(request, response)
	next := []byte(`{"model":"gpt-5.4","instructions":"system","tools":[],"input":[{"role":"user","content":"hello"},{"type":"function_call","call_id":"call-one","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-one","output":"ok"}],"client_metadata":{"turn_id":"two"}}`)
	prepared := state.Prepare(next)
	if gjson.GetBytes(prepared, "previous_response_id").String() != "resp-one" || len(gjson.GetBytes(prepared, "input").Array()) != 1 {
		t.Fatalf("bad delta: %s", prepared)
	}
	for _, path := range []string{"model", "instructions", "input.0.content", "input.1.arguments"} {
		changed, _ := sjson.SetBytes(next, path, "different")
		if gjson.GetBytes(state.Prepare(changed), "previous_response_id").Exists() {
			t.Fatalf("compressed incompatible %s", path)
		}
	}
	newConnection := &CodexContinuation{}
	if gjson.GetBytes(newConnection.Prepare(next), "previous_response_id").Exists() {
		t.Fatal("new connection inherited cached response")
	}
	state.Complete(next, []byte(`{"type":"response.incomplete","response":{"id":"bad","output":[]}}`))
	if gjson.GetBytes(state.Prepare(next), "previous_response_id").Exists() {
		t.Fatal("incomplete response became continuation baseline")
	}
}

func TestCodexWebsocketTransportMatchesNativeSerialization(t *testing.T) {
	body := CodexWebsocketTransportBody([]byte(`{"stream":false,"background":false,"stream_options":{"reasoning_summary_delivery":"auto"},"generate":false,"client_metadata":{"future":"kept"}}`))
	if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "background").Exists() || gjson.GetBytes(body, "stream_options.reasoning_summary_delivery").String() != "auto" || gjson.GetBytes(body, "generate").Bool() {
		t.Fatalf("bad transport fields: %s", body)
	}
	if gjson.GetBytes(body, "client_metadata.future").String() != "kept" {
		t.Fatal("metadata was lost")
	}
}

func TestCodexContinuationDoesNotRoundLargeInputNumbers(t *testing.T) {
	state := &CodexContinuation{}
	state.Complete([]byte(`{"model":"gpt-5.4","input":[{"value":9007199254740992}]}`), []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
	changed := state.Prepare([]byte(`{"model":"gpt-5.4","input":[{"value":9007199254740993}]}`))
	if gjson.GetBytes(changed, "previous_response_id").Exists() {
		t.Fatal("distinct input numbers were compressed into the same context")
	}
}

package chat_completions

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestCodex156NamespaceNamesSurviveChatConversion(t *testing.T) {
	var state any
	added := []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","namespace":"web","name":"run","call_id":"call-1","arguments":""}}`)
	chunks := ConvertCodexResponseToOpenAI(context.Background(), "model", nil, nil, added, &state)
	if len(chunks) != 1 || ojson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.name").String() != "web__run" || ojson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.id").String() != "call-1" {
		t.Fatalf("stream: %s", chunks)
	}
	completed := []byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","namespace":"image_gen","name":"imagegen","call_id":"call-image","arguments":"{\"prompt\":\"draw\"}"}]}}`)
	body := ConvertCodexResponseToOpenAINonStream(context.Background(), "model", nil, nil, completed, nil)
	tool := ojson.GetBytes(body, "choices.0.message.tool_calls.0")
	if tool.Get("function.name").String() != "image_gen__imagegen" || tool.Get("id").String() != "call-image" || tool.Get("function.arguments").String() != `{"prompt":"draw"}` {
		t.Fatalf("nonstream: %s", body)
	}
}

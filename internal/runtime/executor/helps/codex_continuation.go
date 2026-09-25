package helps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexContinuationMaxBytes = 8 << 20

// CodexContinuation is connection-local. It retains a full replayable request,
// while sending a delta only when the complete input prefix and options match.
// This follows codex-rs ModelClientSession::get_incremental_items conservatively:
// unknown property differences always cause a full request, never a guessed delta.
type CodexContinuation struct {
	mu         sync.Mutex
	properties map[string]any
	input      []any
	responseID string
}

func codexContinuationRequest(body []byte) (map[string]any, []any, bool) {
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&request) != nil {
		return nil, nil, false
	}
	input, ok := request["input"].([]any)
	if !ok || gjson.GetBytes(body, "previous_response_id").String() != "" {
		return nil, nil, false
	}
	for _, key := range []string{"input", "client_metadata", "stream", "stream_options", "background", "generate", "type", "previous_response_id"} {
		delete(request, key)
	}
	return request, input, true
}

func (s *CodexContinuation) Prepare(body []byte) []byte {
	if s == nil || len(body) > codexContinuationMaxBytes {
		return body
	}
	properties, input, ok := codexContinuationRequest(body)
	if !ok {
		return body
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responseID == "" || !reflect.DeepEqual(properties, s.properties) || len(input) < len(s.input) || !reflect.DeepEqual(input[:len(s.input)], s.input) {
		return body
	}
	updated, errSet := sjson.SetBytes(body, "previous_response_id", s.responseID)
	if errSet != nil {
		return body
	}
	updated, errSet = sjson.SetBytes(updated, "input", input[len(s.input):])
	if errSet != nil {
		return body
	}
	return updated
}

func (s *CodexContinuation) Complete(request, event []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.properties, s.input, s.responseID = nil, nil, ""
	if len(request)+len(event) > codexContinuationMaxBytes {
		return
	}
	kind := gjson.GetBytes(event, "type").String()
	if kind != "response.completed" && kind != "response.done" {
		return
	}
	properties, input, ok := codexContinuationRequest(request)
	if !ok {
		return
	}
	output := gjson.GetBytes(event, "response.output")
	var items []any
	decoder := json.NewDecoder(strings.NewReader(output.Raw))
	decoder.UseNumber()
	if !output.IsArray() || decoder.Decode(&items) != nil {
		return
	}
	id := gjson.GetBytes(event, "response.id").String()
	if id == "" {
		return
	}
	s.properties, s.input, s.responseID = properties, append(input, items...), id
}

// CodexWebsocketTransportBody follows codex-rs ResponseCreateWsRequest, which
// retains stream and stream_options despite the public API guide's simplified example.
func CodexWebsocketTransportBody(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "background")
	body = SetBoolIfDifferent(body, "stream", true)
	return body
}

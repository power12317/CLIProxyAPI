package basispoints

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Stream decodes SSE incrementally. Only tool items wait for complete arguments;
// text, reasoning, and lifecycle events are emitted as they arrive.
func (b *Bridge) Stream(reader io.Reader, emit func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxItemBytes)
	var data bytes.Buffer
	sequence := 0
	terminal := false
	toolBytes := 0
	pending := make(map[string]bool)
	send := func(event object) error {
		raw, err := eventBytes(event, sequence)
		if err != nil {
			return err
		}
		sequence++
		return emit(raw)
	}
	sendTool := func(item object, index any) error {
		id := stringValue(item["id"])
		if id == "" {
			id = functionItemID(stringValue(item["call_id"]))
		}
		if previous := b.emitted[id]; previous != nil {
			return nil
		}
		converted, err := b.convertItem(item)
		if err != nil {
			return err
		}
		field, event := "arguments", "response.function_call_arguments"
		rawItem, _ := json.Marshal(converted)
		toolBytes += len(rawItem)
		if len(b.emitted) >= 1024 || toolBytes > 32<<20 {
			return failure(502, "tool_output_too_large", "Basispoints tool output exceeds the relay limit")
		}
		if converted["type"] == "custom_tool_call" {
			field, event = "input", "response.custom_tool_call_input"
		}
		added := clone(converted)
		added[field] = ""
		added["status"] = "in_progress"
		if err = send(object{"type": "response.output_item.added", "output_index": index, "item": added}); err != nil {
			return err
		}
		if err = send(object{"type": event + ".delta", "output_index": index, "item_id": id, "delta": converted[field]}); err != nil {
			return err
		}
		if err = send(object{"type": event + ".done", "output_index": index, "item_id": id, field: converted[field]}); err != nil {
			return err
		}
		if err = send(object{"type": "response.output_item.done", "output_index": index, "item": converted}); err != nil {
			return err
		}
		b.emitted[id] = converted
		return nil
	}
	process := func(raw []byte) error {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
			return nil
		}
		if b.ObserveEvent != nil {
			b.ObserveEvent(raw)
		}
		event, err := decode(raw)
		if err != nil {
			return failure(502, "invalid_stream", "Basispoints returned invalid SSE JSON")
		}
		kind := stringValue(event["type"])
		switch kind {
		case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
			// The native output_item.done carries the full original item. Never
			// expose partial Office arguments as a client tool invocation.
			return nil
		case "response.output_item.added":
			if item, ok := event["item"].(object); ok && isTool(item) {
				if id := stringValue(item["id"]); id != "" {
					pending[id] = true
				}
				return nil
			}
		case "response.output_item.done":
			if item, ok := event["item"].(object); ok && isTool(item) {
				if id := stringValue(item["id"]); id != "" {
					pending[id] = true
				}
				return nil
			}
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			response, ok := event["response"].(object)
			if !ok {
				return failure(502, "invalid_stream", "terminal event has no response object")
			}
			items, _ := response["output"].([]any)
			// The complete terminal item is authoritative; output_item.done can
			// contain an unfinished run_officejs envelope. Text still streams live.
			for _, value := range items {
				if item, ok := value.(object); ok && isTool(item) {
					if kind == "response.failed" || kind == "response.incomplete" {
						return failure(502, "incomplete_tools", "Basispoints did not complete the tool response")
					}
					if _, errConvert := b.convertItem(item); errConvert != nil {
						return errConvert
					}
					delete(pending, stringValue(item["id"]))
				}
			}
			if len(pending) != 0 {
				return failure(502, "incomplete_tools", "Basispoints terminal response omitted a tool item")
			}
			for i, value := range items {
				item, ok := value.(object)
				if !ok {
					return failure(502, "invalid_stream", "terminal output item is not an object")
				}
				if !isTool(item) {
					continue
				}
				if kind != "response.failed" && kind != "response.incomplete" {
					if err = sendTool(item, i); err != nil {
						return err
					}
				}
				if converted := b.emitted[stringValue(item["id"])]; converted != nil {
					items[i] = clone(converted)
				} else {
					converted, errConvert := b.convertItem(item)
					if errConvert != nil {
						return errConvert
					}
					items[i] = converted
				}
			}
			terminal = true
			if err = send(event); err != nil {
				return err
			}
			if kind == "response.failed" {
				return StreamError(raw)
			}
			return nil
		case "error":
			return StreamError(raw)
		}
		return send(event)
	}
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		err := process(raw)
		data.Reset()
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			if terminal {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line[5:], " ")
			if data.Len()+len(value)+1 > maxItemBytes {
				return failure(502, "stream_event_too_large", "Basispoints SSE event exceeds the relay limit")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		} else if strings.HasPrefix(line, ":") {
			if err := emit([]byte(line + "\n\n")); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if !terminal {
		return failure(502, "incomplete_stream", "Basispoints stream ended without a terminal response")
	}
	return nil
}

// CompletedResponse extracts the terminal response from a translated SSE event.
func CompletedResponse(event []byte) []byte {
	for _, line := range bytes.Split(event, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var value struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &value) == nil && (value.Type == "response.completed" || value.Type == "response.done" || value.Type == "response.incomplete") {
			return value.Response
		}
	}
	return nil
}

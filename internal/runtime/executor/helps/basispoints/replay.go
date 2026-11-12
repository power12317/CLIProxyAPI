package basispoints

import "strings"

type replayEntry struct {
	raw             []byte
	clientSignature string
	nativeSignature string
}

// Ignore transport IDs/status and JSON object ordering, while retaining every
// business argument, exact custom input, and encryption metadata.
func replaySignature(call object) string {
	kind, id, name := stringValue(call["type"]), stringValue(call["call_id"]), stringValue(call["name"])
	if kind == "tool_search_call" && call["execution"] == "client" {
		name = "tool_search"
	}
	if id == "" || name == "" || strings.TrimSpace(id) != id || strings.TrimSpace(name) != name {
		return ""
	}
	namespace, ok := call["namespace"].(string)
	if call["namespace"] != nil && !ok {
		return ""
	}
	if namespace != "" {
		name = namespace + "." + name
	}
	canonical := object{"type": kind, "call_id": id, "name": name}
	switch kind {
	case "function_call", "tool_search_call":
		var args object
		switch raw := call["arguments"].(type) {
		case object:
			args = raw
		case string:
			var err error
			args, err = decode([]byte(raw))
			if err != nil {
				return ""
			}
		default:
			return ""
		}
		if args == nil {
			return ""
		}
		canonical["arguments"] = args
		if encrypted := call["encrypted_function_args"]; encrypted != nil && digest(encrypted) != digest([]any{}) {
			canonical["encrypted_function_args"] = encrypted
		}
	case "custom_tool_call":
		input, ok := call["input"].(string)
		if !ok {
			return ""
		}
		canonical["input"] = input
	default:
		return ""
	}
	return digest(canonical)
}

func (c *Cache) getForCall(key string, call object) object {
	signature := replaySignature(call)
	if signature == "" {
		return nil
	}
	c.mu.Lock()
	entry := c.items[key]
	c.mu.Unlock()
	if signature != entry.clientSignature && signature != entry.nativeSignature {
		return nil
	}
	item, _ := decode(entry.raw)
	return item
}

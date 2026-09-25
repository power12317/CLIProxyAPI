// Package basispoints bridges Responses clients to the Basispoints protocol.
// Protocol references are recorded in docs/basispoints-integration-design_CN.md.
package basispoints

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexwire"
)

const ResponsesURL = "https://bps.openai.com/basispoints/api/responses"
const maxItemBytes = 8 << 20

type object = map[string]any

// Error preserves upstream status and JSON while identifying request-only failures.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string   { return e.Body }
func (e *Error) StatusCode() int { return e.Status }
func (e *Error) IsRequestScoped() bool {
	return e.Status == 400 || e.Status == 403 || e.Status == 404 || e.Status == 422 || e.Status == 502
}

func failure(status int, code, message string) error {
	body, _ := json.Marshal(object{"error": object{"type": "basispoints_error", "code": code, "message": message}})
	return &Error{Status: status, Body: string(body)}
}

type tool struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Format      any    `json:"format,omitempty"`
	Strict      any    `json:"strict,omitempty"`
}

// Cache retains complete native items. It never contains credentials and is
// bounded by both count and bytes, with no timer-based expiration.
type Cache struct {
	mu    sync.Mutex
	items map[string][]byte
	order []string
	bytes int
}

func (c *Cache) put(key string, item object) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if len(raw) > maxItemBytes {
		return failure(502, "tool_item_too_large", "Basispoints tool item exceeds the relay limit")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string][]byte)
	}
	if previous, ok := c.items[key]; ok {
		c.bytes -= len(previous)
	} else {
		c.order = append(c.order, key)
	}
	c.items[key] = raw
	c.bytes += len(raw)
	for len(c.order) > 1024 || c.bytes > 32<<20 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.items[oldest])
		delete(c.items, oldest)
	}
	return nil
}

func (c *Cache) get(key string) object {
	c.mu.Lock()
	raw := c.items[key]
	c.mu.Unlock()
	item, _ := decode(raw)
	return item
}

// Bridge is request-scoped; the cache is shared across requests and configuration reloads.
type Bridge struct {
	cache   *Cache
	scope   string
	tools   map[string]tool
	choice  string
	emitted map[string]object
	// ObserveEvent receives untouched upstream events for the existing CPA logs.
	ObserveEvent func([]byte)
}

func decode(raw []byte) (object, error) {
	var value object
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, failure(400, "invalid_json", "expected a JSON object")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, failure(400, "invalid_json", "expected exactly one JSON object")
	}
	return value, nil
}

func stringValue(value any) string { text, _ := value.(string); return text }
func clone(item object) object {
	raw, _ := json.Marshal(item)
	value, _ := decode(raw)
	return value
}
func digest(value any) string {
	raw, _ := json.Marshal(value)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func nativeName(name string) bool { return name == "run_officejs" || name == "functions.run_officejs" }

func message(role, text string) object {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return object{"type": "message", "role": role, "content": []any{object{"type": kind, "text": text}}}
}

// Prepare converts an already normalized Responses request without changing its model or effort.
// scope must include the caller and selected credential, never a token.
func Prepare(raw []byte, scope, session string, cache *Cache) ([]byte, *Bridge, error) {
	body, err := decode(raw)
	if err != nil {
		return nil, nil, err
	}
	if cache == nil {
		cache = &Cache{}
	}
	if value := stringValue(body["previous_response_id"]); value != "" {
		return nil, nil, failure(400, "full_history_required", "Basispoints requires full input history instead of previous_response_id")
	}
	bridge := &Bridge{cache: cache, tools: make(map[string]tool), emitted: make(map[string]object)}
	var catalog []tool
	var collect func([]any, string) error
	collect = func(entries []any, namespace string) error {
		for _, value := range entries {
			item, ok := value.(object)
			if !ok {
				return failure(400, "invalid_tool", "tool definitions must be objects")
			}
			kind, name := stringValue(item["type"]), stringValue(item["name"])
			if kind == "namespace" {
				children, _ := item["tools"].([]any)
				if namespace != "" {
					name = namespace + "." + name
				}
				if errCollect := collect(children, name); errCollect != nil {
					return errCollect
				}
				continue
			}
			if kind != "function" && kind != "custom" {
				return failure(400, "unsupported_tool", "Basispoints relay supports function and custom client tools")
			}
			key := name
			if namespace != "" {
				key = namespace + "." + name
			}
			if name == "" || nativeName(key) {
				return failure(400, "invalid_tool", "client tool name is empty or reserved")
			}
			parameters := item["parameters"]
			if parameters == nil {
				parameters = item["inputSchema"]
			}
			if parameters == nil {
				parameters = item["input_schema"]
			}
			entry := tool{Name: name, Namespace: namespace, Type: kind, Description: stringValue(item["description"]), Parameters: parameters, Format: item["format"], Strict: item["strict"]}
			if previous, exists := bridge.tools[key]; exists {
				if digest(previous) != digest(entry) {
					return failure(400, "invalid_tool", "conflicting client tool declaration: "+key)
				}
				continue
			}
			bridge.tools[key] = entry
			catalog = append(catalog, entry)
		}
		return nil
	}
	tools, _ := body["tools"].([]any)
	if err = collect(tools, ""); err != nil {
		return nil, nil, err
	}
	if items, ok := body["input"].([]any); ok {
		for _, value := range items {
			if item, ok := value.(object); ok && item["type"] == "additional_tools" {
				additional, _ := item["tools"].([]any)
				if err = collect(additional, ""); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	switch choice := body["tool_choice"].(type) {
	case string:
		bridge.choice = choice
	case object:
		bridge.choice = stringValue(choice["name"])
		if ns := stringValue(choice["namespace"]); ns != "" {
			bridge.choice = ns + "." + bridge.choice
		}
	}
	if bridge.choice == "required" && len(catalog) == 0 {
		return nil, nil, failure(400, "invalid_tool_choice", "tool_choice requires a client tool")
	}
	if bridge.choice != "" && bridge.choice != "auto" && bridge.choice != "none" && bridge.choice != "required" {
		if _, ok := bridge.tools[bridge.choice]; !ok {
			return nil, nil, failure(400, "invalid_tool_choice", "tool_choice does not identify a client tool")
		}
	}
	var input []any
	switch value := body["input"].(type) {
	case string:
		input = []any{message("user", value)}
	case []any:
		input = value
	case nil:
		input = []any{}
	default:
		return nil, nil, failure(400, "invalid_input", "input must be a string or an array")
	}
	for _, value := range input {
		if item, ok := value.(object); ok {
			if text, isString := item["content"].(string); isString && stringValue(item["role"]) != "" {
				normalized := message(stringValue(item["role"]), text)
				item["content"] = normalized["content"]
				item["type"] = "message"
			}
		}
	}
	if session == "" {
		session = stringValue(body["prompt_cache_key"])
	}
	if session == "" {
		// Stable across tool rounds; include the first user message rather than the injected catalog.
		for _, value := range input {
			if item, ok := value.(object); ok && item["role"] == "user" {
				session = digest(item["content"])
				break
			}
		}
	}
	bridge.scope = digest([]string{scope, session})
	turnIndex, iteration := -1, 1
	var turnContent any
	translatedInput := make([]any, 0, len(input))
	seenCalls := make(map[string]bool)
	for i, value := range input {
		item, ok := value.(object)
		if !ok {
			return nil, nil, failure(400, "invalid_input", "input items must be objects")
		}
		delete(item, "internal_chat_message_metadata_passthrough")
		if item["role"] == "user" {
			turnIndex, iteration, turnContent = i, 1, item["content"]
		}
		kind := stringValue(item["type"])
		if kind == "additional_tools" {
			continue
		}
		if kind == "item_reference" {
			continue
		}
		if kind == "reasoning" {
			if encrypted := stringValue(item["encrypted_content"]); encrypted != "" {
				translatedInput = append(translatedInput, object{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
			}
			continue
		}
		callID := stringValue(item["call_id"])
		if kind == "function_call" || kind == "custom_tool_call" {
			native := cache.get(bridge.scope + "/" + callID)
			if native == nil {
				native, err = rebuildToolCall(item)
				if err != nil {
					return nil, nil, err
				}
				if err = cache.put(bridge.scope+"/"+callID, native); err != nil {
					return nil, nil, err
				}
			}
			item = native
			seenCalls[callID] = true
		}
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			native := cache.get(bridge.scope + "/" + callID)
			if native == nil {
				return nil, nil, failure(400, "tool_replay_missing", "native Basispoints tool item is unavailable; start a new conversation")
			}
			if !seenCalls[callID] {
				translatedInput = append(translatedInput, native)
				seenCalls[callID] = true
			}
			item["type"] = "function_call_output"
			item["id"] = "fc_result_" + digest(callID)[:54]
			if (native["name"] == "update_plan" || native["name"] == "functions.update_plan") && item["output"] == "Plan updated" {
				item["output"] = `{"status":"ok"}`
			}
			delete(item, "name")
			delete(item, "namespace")
			iteration++
		}
		translatedInput = append(translatedInput, item)
	}
	var prologue []any
	if instructions := stringValue(body["instructions"]); instructions != "" {
		prologue = append(prologue, message("developer", instructions))
	}
	instructions := "This is an external Responses client. Do not execute Office or workbook operations. Only tools in the client catalog are available."
	if len(catalog) > 0 && bridge.choice != "none" {
		instructions += toolInstructions(catalog)
		if bridge.choice == "required" {
			instructions += " You must invoke a client tool."
		} else if bridge.choice != "" && bridge.choice != "auto" {
			instructions += " Invoke only this tool: " + bridge.choice
		}
	} else {
		instructions += " Return assistant text; do not call tools."
	}
	prologue = append(prologue, message("developer", instructions))
	// Basispoints uses its own request schema. Build that envelope explicitly;
	// forwarding arbitrary Responses/Codex fields causes HTTP 422 validation errors.
	output := object{
		"model": body["model"], "model_selection": "explicit",
		"stream": body["stream"] == true, "store": false,
		"input": append(prologue, translatedInput...),
	}
	if effort, exists := body["reasoning_effort"]; exists {
		output["reasoning_effort"] = effort
	}
	if key := stringValue(body["prompt_cache_key"]); key != "" {
		output["prompt_cache_key"] = key
	}
	if tier := codexwire.ServiceTier(stringValue(body["service_tier"])); tier != "" {
		output["service_tier"] = tier
	}
	if policy := body["context_management"]; policy != nil {
		if entries, isArray := policy.([]any); !isArray || len(entries) > 0 {
			output["context_management"] = policy
		}
	}
	metadata := requestMetadata(body["metadata"])
	metadata["task_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(bridge.scope)).String()
	metadata["turn_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(bridge.scope+"/"+strconv.Itoa(turnIndex)+"/"+digest(turnContent))).String()
	metadata["agent_iteration"] = strconv.Itoa(iteration)
	output["metadata"] = metadata
	payload, err := json.Marshal(output)
	return payload, bridge, err
}

func (b *Bridge) convertItem(item object) (object, error) {
	if !isTool(item) {
		return item, nil
	}
	return b.convertTool(item)
}

func (b *Bridge) convertResponse(response object) error {
	items, _ := response["output"].([]any)
	for i, value := range items {
		item, ok := value.(object)
		if !ok {
			return failure(502, "invalid_response", "response output item is not an object")
		}
		converted, err := b.convertItem(item)
		if err != nil {
			return err
		}
		items[i] = converted
	}
	return nil
}

// Response translates a non-streaming response while retaining its status and usage.
func (b *Bridge) Response(raw []byte) ([]byte, error) {
	response, err := decode(raw)
	if err != nil {
		return nil, failure(502, "invalid_response", "Basispoints returned invalid JSON")
	}
	if err = b.convertResponse(response); err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

// Scope joins non-secret caller and credential identities without delimiter collisions.
func Scope(parts ...string) string { return digest(parts) }

// StreamError converts an upstream error event without replacing its error payload.
func StreamError(raw []byte) error {
	status := 502
	value, _ := decode(raw)
	if code, ok := value["status"].(json.Number); ok {
		if n, err := code.Int64(); err == nil && n >= 400 && n <= 599 {
			status = int(n)
		}
	}
	return &Error{Status: status, Body: string(raw)}
}

func eventBytes(event object, sequence int) ([]byte, error) {
	event["sequence_number"] = sequence
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", stringValue(event["type"]), raw)), nil
}

func eventType(event object, name string) object {
	result := clone(event)
	result["type"] = name
	return result
}

func isTool(item object) bool {
	return strings.HasSuffix(stringValue(item["type"]), "_call") && (item["type"] == "function_call" || item["type"] == "custom_tool_call")
}

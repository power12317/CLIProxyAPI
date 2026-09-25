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
}

// Cache retains complete native items. It never contains credentials and is
// bounded by both count and bytes, with no timer-based expiration.
type Cache struct {
	mu         sync.Mutex
	items      map[string][]byte
	order      []string
	bytes      int
	owners     map[string]string
	ownerOrder []string
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
	var item object
	_ = json.Unmarshal(raw, &item)
	return item
}

// Bridge is request-scoped; the cache is shared across requests and configuration reloads.
type Bridge struct {
	cache   *Cache
	scope   string
	tools   map[string]tool
	choice  string
	emitted map[string]object
	caller  string
	authID  string
}

// BindCredential enables subsequent tool results to select their original credential.
func (b *Bridge) BindCredential(caller, authID string) { b.caller, b.authID = caller, authID }

// Owner returns the credential that produced tool calls in this caller's history.
// Conflicting histories are left to the request validator rather than repinned.
func (c *Cache) Owner(caller string, payload []byte) string {
	body, err := decode(payload)
	if err != nil {
		return ""
	}
	input, _ := body["input"].([]any)
	owner := ""
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, value := range input {
		item, ok := value.(object)
		if !ok {
			continue
		}
		callID := stringValue(item["call_id"])
		if callID == "" {
			continue
		}
		if id := c.owners[Scope(caller, callID)]; id != "" {
			if owner != "" && owner != id {
				return ""
			}
			owner = id
		}
	}
	return owner
}

func (b *Bridge) rememberOwner(callID string) {
	if b.authID == "" {
		return
	}
	key := Scope(b.caller, callID)
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	if b.cache.owners == nil {
		b.cache.owners = make(map[string]string)
	}
	if _, exists := b.cache.owners[key]; !exists {
		b.cache.ownerOrder = append(b.cache.ownerOrder, key)
	}
	b.cache.owners[key] = b.authID
	for len(b.cache.ownerOrder) > 1024 {
		delete(b.cache.owners, b.cache.ownerOrder[0])
		b.cache.ownerOrder = b.cache.ownerOrder[1:]
	}
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
			if _, exists := bridge.tools[key]; exists {
				return failure(400, "invalid_tool", "duplicate client tool name: "+key)
			}
			entry := tool{Name: name, Namespace: namespace, Type: kind, Description: stringValue(item["description"]), Parameters: item["parameters"]}
			bridge.tools[key] = entry
			catalog = append(catalog, entry)
		}
		return nil
	}
	tools, _ := body["tools"].([]any)
	if err = collect(tools, ""); err != nil {
		return nil, nil, err
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
	bridge.scope = digest([]string{scope, session, stringValue(body["model"])})
	turnIndex, iteration := -1, 1
	var turnContent any
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
		callID := stringValue(item["call_id"])
		if kind == "function_call" || kind == "custom_tool_call" {
			native := cache.get(bridge.scope + "/" + callID)
			if native == nil {
				return nil, nil, failure(400, "tool_replay_missing", "native Basispoints tool item is unavailable; start a new conversation")
			}
			input[i] = native
		}
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			if cache.get(bridge.scope+"/"+callID) == nil {
				return nil, nil, failure(400, "tool_replay_missing", "native Basispoints tool item is unavailable; start a new conversation")
			}
			item["type"] = "function_call_output"
			delete(item, "name")
			delete(item, "namespace")
			iteration++
		}
	}
	var prologue []any
	if instructions := stringValue(body["instructions"]); instructions != "" {
		prologue = append(prologue, message("developer", instructions))
	}
	instructions := "This is an external Responses client. Do not execute Office or workbook operations. Only tools in the client catalog are available."
	if len(catalog) > 0 && bridge.choice != "none" {
		encoded, _ := json.Marshal(catalog)
		instructions += " To invoke a client tool, call run_officejs once per tool. Its code field must be a JSON string encoding exactly one object: {\"tool\":\"namespace.name\",\"args\":{...}}. Use the catalog name without a namespace when none is declared. For custom tools, args is the raw input string. Include summary, extended_summary, destructive=false and references=[] in the outer arguments. The proxy intercepts this call, decodes the JSON, and sends the tool request to the client; code is never executed as JavaScript or OfficeJS. Read replayed outputs as the client's tool results and do not repeat completed calls. Client tool catalog: " + string(encoded)
		if bridge.choice == "required" {
			instructions += " You must invoke a client tool."
		} else if bridge.choice != "" && bridge.choice != "auto" {
			instructions += " Invoke only this tool: " + bridge.choice
		}
	} else {
		instructions += " Return assistant text; do not call tools."
	}
	prologue = append(prologue, message("developer", instructions))
	body["input"] = append(prologue, input...)
	body["model_selection"] = "explicit"
	body["store"] = false
	metadata, _ := body["metadata"].(object)
	if metadata == nil {
		metadata = object{}
	}
	metadata["task_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(bridge.scope)).String()
	metadata["turn_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(bridge.scope+"/"+strconv.Itoa(turnIndex)+"/"+digest(turnContent))).String()
	metadata["agent_iteration"] = strconv.Itoa(iteration)
	body["metadata"] = metadata
	for _, key := range []string{"instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "previous_response_id", "stream_options", "client_metadata", "generate"} {
		delete(body, key)
	}
	if entries, ok := body["context_management"].([]any); ok && len(entries) == 0 {
		delete(body, "context_management")
	}
	payload, err := json.Marshal(body)
	return payload, bridge, err
}

func (b *Bridge) convertItem(item object) (object, error) {
	kind := stringValue(item["type"])
	if kind != "function_call" && kind != "custom_tool_call" {
		return item, nil
	}
	if !nativeName(stringValue(item["name"])) {
		return nil, failure(502, "unexpected_tool", "Basispoints returned a tool outside the client relay")
	}
	arguments, err := decode([]byte(stringValue(item["arguments"])))
	if err != nil {
		return nil, failure(502, "invalid_tool_envelope", "invalid run_officejs arguments")
	}
	envelope, err := decode([]byte(stringValue(arguments["code"])))
	if err != nil {
		return nil, failure(502, "invalid_tool_envelope", "run_officejs code must encode one JSON object")
	}
	name := stringValue(envelope["tool"])
	spec, ok := b.tools[name]
	if !ok || b.choice == "none" || (b.choice != "" && b.choice != "auto" && b.choice != "required" && b.choice != name) {
		return nil, failure(502, "unexpected_tool", "Basispoints returned a tool outside the selected client catalog")
	}
	callID := stringValue(item["call_id"])
	if callID == "" || stringValue(item["id"]) == "" {
		return nil, failure(502, "invalid_tool_envelope", "native tool item is missing its id or call_id")
	}
	result := clone(item)
	result["name"] = spec.Name
	if spec.Namespace != "" {
		result["namespace"] = spec.Namespace
	}
	if spec.Type == "custom" {
		input, ok := envelope["args"].(string)
		if !ok {
			return nil, failure(502, "invalid_tool_arguments", "custom tool input must be a string")
		}
		result["type"], result["input"] = "custom_tool_call", input
		delete(result, "arguments")
	} else {
		args, ok := envelope["args"].(object)
		if !ok {
			return nil, failure(502, "invalid_tool_arguments", "function tool arguments must be an object")
		}
		encoded, errMarshal := json.Marshal(args)
		if errMarshal != nil {
			return nil, errMarshal
		}
		result["type"], result["arguments"] = "function_call", string(encoded)
	}
	if err = b.cache.put(b.scope+"/"+callID, item); err != nil {
		return nil, err
	}
	b.rememberOwner(callID)
	return result, nil
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

package helps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	session "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
)

// PrismEndpoint shares the adapter's deployment port without a product setting.
func PrismEndpoint() (string, error) {
	port := strings.TrimSpace(os.Getenv("PRISM_ADAPTER_PORT"))
	if port == "" {
		port = "8319"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1024 || number > 65535 {
		return "", NewPrismError(http.StatusBadGateway, "prism_unavailable", "Prism adapter port is invalid")
	}
	return "http://127.0.0.1:" + strconv.Itoa(number) + "/v1/responses", nil
}

// PrismError ends the request without protocol fallback or credential rotation.
type PrismError struct {
	Status int
	Body   string
}

func (e PrismError) Error() string         { return e.Body }
func (e PrismError) StatusCode() int       { return e.Status }
func (e PrismError) IsRequestScoped() bool { return true }
func (e PrismError) IsRequestStop() bool   { return true }
func NewPrismError(status int, code, message string) error {
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"type": code, "message": message}})
	return PrismError{Status: status, Body: string(data)}
}

func prismDigest(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PrismIdentity binds the private bridge headers to authenticated caller context.
// Browser/project reuse only uses explicit client identity, never inferred history.
func PrismIdentity(auth *coreauth.Auth, req core.Request, opts core.Options) (account, caller, conversation string) {
	account = prismDigest("prism-account-v1", auth.ID, CodexOAuthAccountID(auth))
	scope, _ := opts.Metadata[core.CallerScopeMetadataKey].(string)
	if scope == "" {
		scope, _ = req.Metadata[core.CallerScopeMetadataKey].(string)
	}
	if scope == "" {
		return
	}
	caller = prismDigest("prism-caller-v1", account, scope)
	body := opts.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	headers := opts.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	// A Codex thread is more specific than a process-wide session header.
	thread := headers.Get("X-Thread-Id")
	if thread == "" {
		thread = headers.Get("Thread-Id")
	}
	for _, path := range []string{"client_metadata.thread_id", "client_metadata.conversation_id", "thread_id", "conversation_id"} {
		if thread == "" {
			thread = gjson.GetBytes(body, path).String()
		}
	}
	if thread != "" {
		conversation = prismDigest("prism-session-v1", caller, "thread", thread)
		return
	}
	for _, pair := range [][2]string{{"session_id", "Session-Id"}, {"conversation_id", "X-Conversation-Id"}} {
		if headers.Get(pair[1]) == "" {
			if value := gjson.GetBytes(body, "client_metadata."+pair[0]).String(); value != "" {
				headers.Set(pair[1], value)
			}
		}
	}
	if info, ok := session.ExtractSessionInfo(headers, body, nil); ok && info.SessionID != "" {
		conversation = prismDigest("prism-session-v1", caller, info.SessionID)
	}
	return
}

// PrismTerminal consumes the buffered adapter reply. All downstream events are
// reconstructed from this completed response so tool identities stay consistent.
func PrismTerminal(body []byte, model string, stream bool, request []byte) ([]byte, error) {
	terminal := body
	fail := func() ([]byte, error) {
		return nil, NewPrismError(502, "invalid_prism_response", "Prism adapter returned an invalid terminal response")
	}
	if stream {
		terminal = nil
		for _, line := range bytes.Split(body, []byte("\n")) {
			if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
			if !gjson.ValidBytes(data) {
				return fail()
			}
			if gjson.GetBytes(data, "type").String() == "response.completed" {
				if terminal != nil {
					return fail()
				}
				terminal = []byte(gjson.GetBytes(data, "response").Raw)
			}
		}
	}
	if !gjson.ValidBytes(terminal) {
		return fail()
	}
	root := gjson.ParseBytes(terminal)
	if root.Get("id").String() == "" || root.Get("status").String() != "completed" || root.Get("model").String() != model || root.Get("usage").Type != gjson.Null {
		return fail()
	}
	output := root.Get("output")
	if !output.IsArray() || len(output.Array()) == 0 {
		return fail()
	}
	catalog := map[string]string{}
	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, ns string) {
		for _, tool := range tools.Array() {
			name := tool.Get("name").String()
			kind := tool.Get("type").String()
			full := name
			if ns != "" {
				full = ns + "." + name
			}
			if kind == "namespace" {
				collect(tool.Get("tools"), full)
			} else if kind == "function" || kind == "custom" {
				catalog[full] = kind
			}
		}
	}
	collect(gjson.GetBytes(request, "tools"), "")
	collect(gjson.GetBytes(request, "additional_tools"), "")
	for _, item := range gjson.GetBytes(request, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			collect(item.Get("tools"), "")
		}
	}
	seen := map[string]bool{}
	for _, item := range output.Array() {
		if item.Get("id").String() == "" {
			return fail()
		}
		switch item.Get("type").String() {
		case "message":
			content := item.Get("content").Array()
			if len(content) == 0 {
				return fail()
			}
			for _, part := range content {
				if part.Get("type").String() != "output_text" || part.Get("text").Type != gjson.String {
					return fail()
				}
			}
		case "function_call", "custom_tool_call":
			kind := "function"
			if item.Get("type").String() == "custom_tool_call" {
				kind = "custom"
			}
			id := item.Get("call_id").String()
			name := item.Get("name").String()
			if ns := item.Get("namespace").String(); ns != "" {
				name = ns + "." + name
			}
			if id == "" || seen[id] || catalog[name] != kind {
				return fail()
			}
			seen[id] = true
			if kind == "function" {
				a := item.Get("arguments")
				if a.Type != gjson.String || !gjson.Valid(a.String()) || !gjson.Parse(a.String()).IsObject() {
					return fail()
				}
			} else if item.Get("input").Type != gjson.String {
				return fail()
			}
		default:
			return fail()
		}
	}
	return terminal, nil
}

// PrismEvents matches sub2api's completed-item SSE transport, without token deltas.
func PrismEvents(terminal []byte) []byte {
	var response map[string]any
	_ = json.Unmarshal(terminal, &response)
	created := map[string]any{}
	for k, v := range response {
		created[k] = v
	}
	created["status"] = "in_progress"
	created["output"] = []any{}
	var out bytes.Buffer
	sequence := 0
	emit := func(kind string, fields map[string]any) {
		fields["type"] = kind
		fields["sequence_number"] = sequence
		sequence++
		raw, _ := json.Marshal(fields)
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", kind, raw)
	}
	emit("response.created", map[string]any{"response": created})
	emit("response.in_progress", map[string]any{"response": created})
	for index, raw := range response["output"].([]any) {
		item := raw.(map[string]any)
		added := map[string]any{}
		for k, v := range item {
			added[k] = v
		}
		added["status"] = "in_progress"
		kind := item["type"].(string)
		switch kind {
		case "message":
			added["content"] = []any{}
		case "function_call":
			added["arguments"] = ""
		default:
			added["input"] = ""
		}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		fields := func() map[string]any { return map[string]any{"output_index": index, "item_id": item["id"]} }
		switch kind {
		case "message":
			for ci, p := range item["content"].([]any) {
				part := p.(map[string]any)
				empty := map[string]any{}
				for k, v := range part {
					empty[k] = v
				}
				empty["text"] = ""
				f := fields()
				f["content_index"] = ci
				f["part"] = empty
				emit("response.content_part.added", f)
				f = fields()
				f["content_index"] = ci
				f["text"] = part["text"]
				emit("response.output_text.done", f)
				f = fields()
				f["content_index"] = ci
				f["part"] = part
				emit("response.content_part.done", f)
			}
		case "function_call":
			f := fields()
			f["arguments"] = item["arguments"]
			emit("response.function_call_arguments.done", f)
		default:
			f := fields()
			f["input"] = item["input"]
			emit("response.custom_tool_call_input.done", f)
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	emit("response.completed", map[string]any{"response": response})
	return out.Bytes()
}

// PrismAdapterError distinguishes local bridge setup failures from upstream failures.
func PrismAdapterError(status int, body []byte) error {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if code == "" {
		code = "prism_unavailable"
	}
	if message == "" {
		message = "Prism adapter returned an error"
	}
	if (status == 401 || status == 403 || status == 404 || status == 405) && code != "project_edit_access_required" {
		status = 502
		code = "prism_unavailable"
		message = "Prism adapter rejected the gateway; check its key and endpoint"
	}
	return NewPrismError(status, code, message)
}

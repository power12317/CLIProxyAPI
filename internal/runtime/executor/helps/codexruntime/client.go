// Package codexruntime implements the versioned, request-scoped Codex bridge.
package codexruntime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

const ProtocolVersion = 1
const MaxMessageBytes = 32 << 20

// Error is deliberately request-scoped: an ambiguous inference cannot be replayed.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string              { return e.Message }
func (e *Error) StatusCode() int            { return e.Status }
func (e *Error) IsRequestScoped() bool      { return true }
func (e *Error) IsCodexReplayUnsafe() bool  { return true }
func fail(status int, message string) error { return &Error{Status: status, Message: message} }

// Capabilities binds one connection to one managed credential and account.
type Capabilities struct {
	ProtocolVersion    int      `json:"protocolVersion"`
	RuntimeVersion     string   `json:"runtimeVersion"`
	UpstreamRevision   string   `json:"upstreamRevision"`
	CredentialID       string   `json:"credentialId"`
	AccountID          string   `json:"accountId"`
	AuthMode           string   `json:"authMode"`
	ExecutionMode      string   `json:"executionMode"`
	RawEvents          bool     `json:"rawEvents"`
	Operations         []string `json:"operations"`
	PersistentSessions bool     `json:"persistentSessions"`
}

type Request struct {
	RequestID    string          `json:"requestId"`
	CredentialID string          `json:"credentialId"`
	AccountID    string          `json:"accountId"`
	Operation    string          `json:"operation"`
	SourceFormat string          `json:"sourceFormat"`
	SessionID    string          `json:"sessionId"`
	Request      json.RawMessage `json:"request"`
}

type Frame struct {
	Method string
	Event  json.RawMessage
	Body   json.RawMessage
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		HTTPStatus int `json:"httpStatus"`
	} `json:"data"`
}
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type Client struct {
	conn       *websocket.Conn
	ctx        context.Context
	stop       func() bool
	once       sync.Once
	nextID     int
	requestID  string
	terminal   bool
	resultSeen bool
	done       bool
	operation  string
	Caps       Capabilities
	Headers    http.Header
}

// Open uses a local Unix socket, without proxy environment variables or deadlines.
func Open(ctx context.Context, socket, credentialID, accountID string) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(socket) {
		return nil, fail(503, "Codex runtime requires an absolute Unix socket")
	}
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	conn, resp, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail(503, "Codex runtime connection failed")
	}
	c := &Client{conn: conn, ctx: ctx}
	conn.SetReadLimit(MaxMessageBytes)
	c.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	if err = c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cliproxyapi", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return nil, err
	}
	if err = c.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err = c.call("cpa/capabilities/read", map[string]any{}, &c.Caps); err != nil {
		return nil, err
	}
	caps := c.Caps
	if caps.ProtocolVersion != ProtocolVersion || caps.ExecutionMode != "inference-only" || !caps.RawEvents || caps.RuntimeVersion == "" || caps.UpstreamRevision == "" {
		return nil, fail(502, "Codex runtime protocol or capabilities mismatch")
	}
	if caps.CredentialID != credentialID {
		return nil, fail(409, "Codex runtime credential mismatch")
	}
	if caps.AuthMode != "chatgpt" || caps.AccountID == "" {
		return nil, fail(401, "Codex runtime requires managed ChatGPT login")
	}
	if caps.AccountID != accountID {
		return nil, fail(409, "Codex runtime account mismatch")
	}
	ok = true
	return c, nil
}

func (c *Client) Close() {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		_ = c.conn.Close()
	})
}
func (c *Client) ioError() error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	return fail(502, "Codex runtime connection closed before completion")
}
func (c *Client) write(v any) error {
	if err := c.conn.WriteJSON(v); err != nil {
		return c.ioError()
	}
	return nil
}
func (c *Client) read() (message, error) {
	var m message
	if err := c.conn.ReadJSON(&m); err != nil {
		return m, c.ioError()
	}
	return m, nil
}
func (c *Client) call(method string, params, out any) error {
	c.nextID++
	id := c.nextID
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for i := 0; i < 128; i++ {
		m, err := c.read()
		if err != nil {
			return err
		}
		if len(m.ID) == 0 {
			if strings.HasPrefix(m.Method, "cpa/inference/") {
				return fail(502, "Codex runtime sent inference data before acceptance")
			}
			continue
		}
		var got int
		if json.Unmarshal(m.ID, &got) != nil || got != id || m.Method != "" {
			return fail(502, "Unexpected Codex runtime RPC response")
		}
		if m.Error != nil {
			status := m.Error.Data.HTTPStatus
			if status < 400 || status > 599 {
				status = 502
			}
			return fail(status, "Codex runtime rejected "+method)
		}
		if len(m.Result) == 0 {
			return fail(502, "Codex runtime RPC result missing")
		}
		if out != nil && json.Unmarshal(m.Result, out) != nil {
			return fail(502, "Invalid Codex runtime RPC result")
		}
		return nil
	}
	return fail(502, "Excessive unrelated Codex runtime notifications")
}

func (c *Client) Start(req Request) error {
	if c.requestID != "" {
		return fail(409, "Codex runtime connection already used")
	}
	if req.RequestID == "" || req.SessionID == "" {
		return fail(400, "Codex runtime request and session identity are required")
	}
	if req.CredentialID != c.Caps.CredentialID || req.AccountID != c.Caps.AccountID {
		return fail(409, "Codex runtime request identity mismatch")
	}
	supported := false
	for _, op := range c.Caps.Operations {
		if op == req.Operation {
			supported = true
		}
	}
	if !supported {
		return fail(400, "Codex runtime operation is unsupported")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(req.Request, &body) != nil || body == nil {
		return fail(400, "Codex runtime request must be a JSON object")
	}
	if !c.Caps.PersistentSessions {
		if v := body["previous_response_id"]; len(v) > 0 && string(v) != "null" && string(v) != "\"\"" {
			return fail(400, "Codex runtime does not support previous_response_id")
		}
		if string(body["generate"]) == "false" {
			return fail(400, "Codex runtime does not support prewarm")
		}
	}
	var result struct {
		RequestID  string      `json:"requestId"`
		StatusCode int         `json:"statusCode"`
		Headers    http.Header `json:"headers"`
	}
	if err := c.call("cpa/inference/start", req, &result); err != nil {
		return err
	}
	if result.RequestID != req.RequestID || result.StatusCode < 200 || result.StatusCode > 299 {
		return fail(502, "Invalid Codex runtime inference acceptance")
	}
	c.requestID, c.operation, c.Headers = req.RequestID, req.Operation, SafeHeaders(result.Headers)
	return nil
}

// Next returns ordered complete wire events; it never reconstructs lost fields.
func (c *Client) Next() (Frame, error) {
	if c.done {
		return Frame{}, io.EOF
	}
	if c.requestID == "" {
		return Frame{}, fail(409, "Codex runtime inference has not started")
	}
	for skipped := 0; skipped < 128; skipped++ {
		m, err := c.read()
		if err != nil {
			return Frame{}, err
		}
		if !strings.HasPrefix(m.Method, "cpa/inference/") {
			continue
		}
		var p struct {
			RequestID  string          `json:"requestId"`
			Event      json.RawMessage `json:"event"`
			Body       json.RawMessage `json:"body"`
			HTTPStatus int             `json:"httpStatus"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.RequestID != c.requestID {
			return Frame{}, fail(502, "Codex runtime event request identity mismatch")
		}
		switch m.Method {
		case "cpa/inference/completed":
			c.done = true
			if !c.terminal && !c.resultSeen {
				return Frame{}, fail(502, "Codex runtime completed without a terminal response")
			}
			return Frame{}, io.EOF
		case "cpa/inference/error":
			c.done = true
			status := p.HTTPStatus
			if status < 400 || status > 599 {
				status = 502
			}
			return Frame{}, fail(status, "Codex runtime inference failed")
		case "cpa/inference/result":
			if c.operation != "responses/compact" || c.resultSeen || !isObject(p.Body) {
				return Frame{}, fail(502, "Unexpected Codex runtime result")
			}
			c.resultSeen = true
			return Frame{Method: m.Method, Body: p.Body}, nil
		case "cpa/inference/event":
			if c.operation != "responses" || c.terminal {
				return Frame{}, fail(502, "Unexpected Codex runtime event after terminal")
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(p.Event, &event) != nil || event.Type == "" {
				return Frame{}, fail(502, "Invalid Codex runtime wire event")
			}
			switch event.Type {
			case "error":
				c.terminal = true
			case "response.completed", "response.incomplete", "response.failed":
				if !isObject(event.Response) {
					return Frame{}, fail(502, "Codex runtime terminal response is missing")
				}
				c.terminal = true
			}
			return Frame{Method: m.Method, Event: p.Event}, nil
		default:
			return Frame{}, fail(502, "Unknown Codex runtime inference notification")
		}
	}
	return Frame{}, fail(502, "Excessive unrelated Codex runtime notifications")
}

// SafeHeaders uses an allowlist to keep cookies, auth, and transport headers private.
func SafeHeaders(headers http.Header) http.Header {
	out := make(http.Header)
	for k, v := range headers {
		lower := strings.ToLower(k)
		if lower == "content-type" || lower == "x-request-id" || lower == "openai-model" || lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return out
}

func isObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

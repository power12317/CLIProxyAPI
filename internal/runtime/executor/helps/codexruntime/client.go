// Package codexruntime implements the versioned, request-scoped Codex bridge.
package codexruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

const ProtocolVersion = 3

// Error is deliberately request-scoped: an ambiguous inference cannot be replayed.
type Error struct {
	Status          int
	Message         string
	Body            []byte
	ResponseHeaders http.Header
}

func (e *Error) Error() string              { return e.Message }
func (e *Error) StatusCode() int            { return e.Status }
func (e *Error) IsRequestScoped() bool      { return true }
func (e *Error) IsCodexReplayUnsafe() bool  { return true }
func (e *Error) ResponseBody() []byte       { return e.Body }
func (e *Error) Headers() http.Header       { return e.ResponseHeaders.Clone() }
func fail(status int, message string) error { return &Error{Status: status, Message: message} }

// Capabilities describes the master shared by all managed credentials.
type Capabilities struct {
	ProtocolVersion  int      `json:"protocolVersion"`
	RuntimeVersion   string   `json:"runtimeVersion"`
	UpstreamRevision string   `json:"upstreamRevision"`
	ExecutionMode    string   `json:"executionMode"`
	RawBody          bool     `json:"rawBody"`
	Operations       []string `json:"operations"`
	ManualOAuth      bool     `json:"manualOAuth"`
	UpstreamLogs     bool     `json:"upstreamLogs"`
	UpstreamBodyLogs bool     `json:"upstreamBodyLogs"`
}

// UpstreamLog describes an actual HTTP attempt made by the worker.
type UpstreamLog struct {
	RequestID         string      `json:"requestId"`
	Kind              string      `json:"kind"`
	URL               string      `json:"url"`
	Method            string      `json:"method"`
	Headers           http.Header `json:"headers"`
	Body              string      `json:"body"`
	BodyBytes         []byte      `json:"bodyBase64"`
	StatusCode        int         `json:"statusCode"`
	Message           string      `json:"message"`
	AccessTokenSHA256 string      `json:"accessTokenSha256"`
	OaiLBNode         string      `json:"oaiLbNode"`
}

type Request struct {
	RequestID    string          `json:"requestId"`
	CredentialID string          `json:"credentialId"`
	Operation    string          `json:"operation"`
	SourceFormat string          `json:"sourceFormat"`
	SessionID    string          `json:"sessionId"`
	Request      json.RawMessage `json:"request"`
}

type Frame struct {
	Body []byte
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		HTTPStatus int         `json:"httpStatus"`
		Body       string      `json:"body"`
		Headers    http.Header `json:"headers"`
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
	conn             *websocket.Conn
	ctx              context.Context
	stop             func() bool
	once             sync.Once
	onClose          func()
	nextID           int
	requestID        string
	done             bool
	pending          []message
	Caps             Capabilities
	Headers          http.Header
	StatusCode       int
	OnUpstream       func(UpstreamLog)
	upstreamResponse *UpstreamLog
}

// Dial opens the internal TCP bridge for inference or explicit management calls.
func Dial(ctx context.Context, endpoint string) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
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
	// The local runtime bridge carries complete requests and upstream logs without a size limit.
	c.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cliproxyapi", "version": "3"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.call("cpa/capabilities/read", map[string]any{}, &c.Caps); err != nil {
		c.Close()
		return nil, err
	}
	if c.Caps.ProtocolVersion != ProtocolVersion || c.Caps.ExecutionMode != "inference-only" || !c.Caps.RawBody {
		c.Close()
		return nil, fail(502, "Codex runtime protocol or capabilities mismatch")
	}
	return c, nil
}

func (c *Client) Call(method string, params, out any) error { return c.call(method, params, out) }

func (c *Client) Close() {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		_ = c.conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
}

func (c *Client) OnClose(fn func()) { c.onClose = fn }
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
	for skipped := 0; skipped < 128; {
		m, err := c.read()
		if err != nil {
			return err
		}
		if len(m.ID) == 0 {
			if strings.HasPrefix(m.Method, "cpa/inference/") {
				if m.Method == "cpa/inference/upstream" && len(c.pending) == 0 {
					if err := c.observeUpstream(m.Params); err != nil {
						return err
					}
				} else {
					c.pending = append(c.pending, m)
				}
				continue
			}
			skipped++
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
			return c.remoteError(status, m.Error.Message, m.Error.Data.Body, m.Error.Data.Headers)
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
	if req.RequestID == "" || req.CredentialID == "" || req.SessionID == "" {
		return fail(400, "Codex runtime request, credential and session identity are required")
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
	if v := body["previous_response_id"]; len(v) > 0 && string(v) != "null" && string(v) != "\"\"" {
		return fail(400, "Codex runtime does not support previous_response_id")
	}
	if string(body["generate"]) == "false" {
		return fail(400, "Codex runtime does not support prewarm")
	}
	var result struct {
		RequestID  string      `json:"requestId"`
		StatusCode int         `json:"statusCode"`
		Headers    http.Header `json:"headers"`
	}
	c.requestID = req.RequestID
	if err := c.call("cpa/inference/start", req, &result); err != nil {
		return err
	}
	if result.RequestID != req.RequestID || result.StatusCode < 200 || result.StatusCode > 299 {
		return fail(502, "Invalid Codex runtime inference acceptance")
	}
	c.StatusCode, c.Headers = result.StatusCode, SafeHeaders(result.Headers)
	return nil
}

// Next returns original HTTP body chunks without parsing Responses events.
func (c *Client) Next() (Frame, error) {
	if c.done {
		return Frame{}, io.EOF
	}
	if c.requestID == "" {
		return Frame{}, fail(409, "Codex runtime inference has not started")
	}
	for skipped := 0; skipped < 128; {
		if err := c.ctx.Err(); err != nil {
			return Frame{}, err
		}
		var m message
		if len(c.pending) > 0 {
			m, c.pending = c.pending[0], c.pending[1:]
		} else {
			var err error
			m, err = c.read()
			if err != nil {
				return Frame{}, err
			}
		}
		if !strings.HasPrefix(m.Method, "cpa/inference/") {
			skipped++
			continue
		}
		if m.Method == "cpa/inference/upstream" {
			if err := c.observeUpstream(m.Params); err != nil {
				return Frame{}, err
			}
			continue
		}
		var p struct {
			RequestID  string          `json:"requestId"`
			BodyBytes  []byte          `json:"bodyBase64"`
			Body       json.RawMessage `json:"body"`
			HTTPStatus int             `json:"httpStatus"`
			Message    string          `json:"message"`
			Headers    http.Header     `json:"headers"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.RequestID != c.requestID {
			return Frame{}, fail(502, "Codex runtime body request identity mismatch")
		}
		switch m.Method {
		case "cpa/inference/completed":
			c.done = true
			return Frame{}, io.EOF
		case "cpa/inference/error":
			c.done = true
			status := p.HTTPStatus
			if status < 400 || status > 599 {
				status = 502
			}
			var body string
			if len(p.Body) > 0 {
				if json.Unmarshal(p.Body, &body) != nil {
					body = string(p.Body)
				}
			}
			return Frame{}, c.remoteError(status, p.Message, body, p.Headers)
		case "cpa/inference/body":
			if c.OnUpstream != nil {
				c.OnUpstream(UpstreamLog{RequestID: p.RequestID, Kind: "body", BodyBytes: p.BodyBytes})
			}
			return Frame{Body: p.BodyBytes}, nil
		default:
			return Frame{}, fail(502, "Unknown Codex runtime inference notification")
		}
	}
	return Frame{}, fail(502, "Excessive unrelated Codex runtime notifications")
}

func (c *Client) observeUpstream(raw json.RawMessage) error {
	var entry UpstreamLog
	if json.Unmarshal(raw, &entry) != nil || c.requestID == "" || entry.RequestID != c.requestID {
		return fail(502, "Codex runtime upstream log request identity mismatch")
	}
	entry.Headers = canonicalHeaders(entry.Headers)
	switch entry.Kind {
	case "request":
		c.upstreamResponse = nil
	case "response":
		c.upstreamResponse = &entry
	case "error":
	default:
		return fail(502, "Unknown Codex runtime upstream log kind")
	}
	if c.OnUpstream != nil {
		c.OnUpstream(entry)
	}
	return nil
}

func (c *Client) remoteError(status int, message, body string, headers http.Header) error {
	if last := c.upstreamResponse; last != nil && last.StatusCode >= 400 {
		status = last.StatusCode
		if body == "" {
			body = last.Body
		}
		if len(headers) == 0 {
			headers = last.Headers
		}
	}
	if body != "" {
		message = body
	}
	if message == "" {
		message = "Codex runtime inference failed"
	}
	return &Error{Status: status, Message: message, Body: []byte(body), ResponseHeaders: SafeHeaders(headers)}
}

func canonicalHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	for k, values := range headers {
		for _, value := range values {
			out.Add(k, value)
		}
	}
	return out
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

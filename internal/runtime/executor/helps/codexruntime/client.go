// Package codexruntime implements the versioned, request-scoped Codex bridge.
package codexruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

const ProtocolVersion = 5

// Error preserves native failures for CPA's ordinary retry policy. IPC failures
// remain request-scoped because their upstream outcome is unknown.
type Error struct {
	Status          int
	Message         string
	Body            []byte
	ResponseHeaders http.Header
	NativeError     json.RawMessage
	nativeFailure   bool
}

func (e *Error) Error() string              { return e.Message }
func (e *Error) StatusCode() int            { return e.Status }
func (e *Error) IsRequestScoped() bool      { return !e.nativeFailure }
func (e *Error) IsCodexReplayUnsafe() bool  { return !e.nativeFailure }
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

// UpstreamLog describes an actual native transport attempt made by the worker.
type UpstreamLog struct {
	RequestID         string      `json:"requestId"`
	Kind              string      `json:"kind"`
	Transport         string      `json:"transport"`
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
	ImageAPI     string          `json:"imageApi,omitempty"`
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
		HTTPStatus  int             `json:"httpStatus"`
		Body        string          `json:"body"`
		Headers     http.Header     `json:"headers"`
		NativeError json.RawMessage `json:"nativeError"`
	} `json:"data"`
}
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type incoming struct {
	message message
	err     error
}

type Client struct {
	inbox           *notificationQueue[incoming]
	diagnostics     *notificationQueue[UpstreamLog]
	diagnosticsDone chan struct{}
	diagnosticsOnce sync.Once
	receiving       atomic.Bool
	finalReceived   atomic.Bool
	nativeSuccess   atomic.Bool
	conn            *websocket.Conn
	ctx             context.Context
	stop            func() bool
	once            sync.Once
	onClose         func()
	nextID          int
	requestID       string
	done            bool
	pending         []message
	Caps            Capabilities
	Headers         http.Header
	StatusCode      int
	OnUpstream      func(UpstreamLog)
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
	c := &Client{conn: conn, ctx: ctx, diagnosticsDone: make(chan struct{})}
	// The local runtime bridge carries complete requests and upstream logs without a size limit.
	c.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cliproxyapi", "version": "5"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
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
		return nil, fail(502, fmt.Sprintf("Codex runtime protocol or capabilities mismatch (requires protocol %d, got %d); upgrade CPA and codex-server together", ProtocolVersion, c.Caps.ProtocolVersion))
	}
	return c, nil
}

func (c *Client) Call(method string, params, out any) error { return c.call(method, params, out) }

func (c *Client) Close() {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		// A confirmed native result is independent of diagnostic delivery.
		// Keep reading trailing logs until their own completion marker.
		if !c.finalReceived.Load() {
			_ = c.conn.Close()
		}
		if !c.receiving.Load() {
			c.finishDiagnostics()
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
}

func (c *Client) OnClose(fn func()) { c.onClose = fn }

func (c *Client) NativeCompleted() bool { return c.nativeSuccess.Load() }
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
	if c.inbox != nil {
		item, ok := c.inbox.next(c.ctx)
		if !ok {
			return message{}, c.ioError()
		}
		return item.message, item.err
	}
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
				c.pending = append(c.pending, m)
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
			return c.remoteError(status, m.Error.Message, m.Error.Data.Body, m.Error.Data.Headers, m.Error.Data.NativeError)
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
	var result struct {
		RequestID  string      `json:"requestId"`
		StatusCode int         `json:"statusCode"`
		Headers    http.Header `json:"headers"`
	}
	c.requestID = req.RequestID
	c.startReceiving(c.nextID + 1)
	if err := c.call("cpa/inference/start", req, &result); err != nil {
		return err
	}
	if result.RequestID != req.RequestID || result.StatusCode < 200 || result.StatusCode > 299 {
		return fail(502, "Invalid Codex runtime inference acceptance")
	}
	c.StatusCode, c.Headers = result.StatusCode, SafeHeaders(result.Headers)
	return nil
}

// Next returns native output bytes without interpreting model events.
func (c *Client) Next() (Frame, error) {
	if c.done {
		return Frame{}, io.EOF
	}
	if c.requestID == "" {
		return Frame{}, fail(409, "Codex runtime inference has not started")
	}
	for skipped := 0; skipped < 128; {
		if err := c.ctx.Err(); err != nil && !c.finalReceived.Load() {
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
		var p struct {
			RequestID   string          `json:"requestId"`
			BodyBytes   []byte          `json:"bodyBase64"`
			Body        json.RawMessage `json:"body"`
			HTTPStatus  int             `json:"httpStatus"`
			Message     string          `json:"message"`
			Headers     http.Header     `json:"headers"`
			NativeError json.RawMessage `json:"nativeError"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.RequestID != c.requestID {
			return Frame{}, fail(502, "Codex runtime body request identity mismatch")
		}
		switch m.Method {
		case "cpa/inference/event":
			return Frame{Body: p.BodyBytes}, nil
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
			return Frame{}, c.remoteError(status, p.Message, body, p.Headers, p.NativeError)
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
	case "request", "response", "error", "body":
	default:
		return fail(502, "Unknown Codex runtime upstream log kind")
	}
	c.diagnostics.push(entry)
	return nil
}

// WaitDiagnostics joins log consumption only. Call after business delivery, never
// before returning a frame or final model result to the HTTP consumer.
func (c *Client) WaitDiagnostics() {
	<-c.diagnosticsDone
}

func (c *Client) finishDiagnostics() {
	c.diagnosticsOnce.Do(func() { close(c.diagnosticsDone) })
}

func (c *Client) startReceiving(startID int) {
	c.receiving.Store(true)
	c.inbox = newNotificationQueue[incoming]()
	c.diagnostics = newNotificationQueue[UpstreamLog]()
	observer := c.OnUpstream
	go func() {
		defer c.finishDiagnostics()
		for {
			entry, ok := c.diagnostics.next(context.Background())
			if !ok {
				return
			}
			if observer != nil {
				observer(entry)
			}
		}
	}()
	go func() {
		defer c.inbox.close()
		defer c.diagnostics.close()
		defer func() { _ = c.conn.Close() }()
		diagnosticsFinished := false
		for {
			var m message
			if err := c.conn.ReadJSON(&m); err != nil {
				c.inbox.push(incoming{err: c.ioError()})
				return
			}
			if m.Method == "cpa/inference/upstream" {
				if err := c.observeUpstream(m.Params); err != nil {
					c.inbox.push(incoming{err: err})
					return
				}
				continue
			}
			if m.Method == "cpa/inference/diagnostics/completed" {
				var p struct {
					RequestID string `json:"requestId"`
				}
				if json.Unmarshal(m.Params, &p) != nil || p.RequestID != c.requestID {
					c.inbox.push(incoming{err: fail(502, "Codex runtime diagnostics request identity mismatch")})
					return
				}
				diagnosticsFinished = true
			} else {
				var responseID int
				if m.Error != nil && json.Unmarshal(m.ID, &responseID) == nil && responseID == startID {
					c.finalReceived.Store(true)
					if c.stop != nil {
						c.stop()
					}
				}
				if m.Method == "cpa/inference/completed" || m.Method == "cpa/inference/error" {
					var p struct {
						RequestID string `json:"requestId"`
					}
					if json.Unmarshal(m.Params, &p) == nil && p.RequestID == c.requestID {
						c.finalReceived.Store(true)
						c.nativeSuccess.Store(m.Method == "cpa/inference/completed")
						if c.stop != nil {
							c.stop()
						}
					}
				}
				c.inbox.push(incoming{message: m})
			}
			if diagnosticsFinished && c.finalReceived.Load() {
				return
			}
		}
	}()
}

func (c *Client) remoteError(status int, message, body string, headers http.Header, nativeError json.RawMessage) error {
	// Attempt diagnostics cannot override the native executor's final outcome.
	if body != "" {
		message = body
	}
	if message == "" {
		message = "Codex runtime inference failed"
	}
	return &Error{Status: status, Message: message, Body: []byte(body), ResponseHeaders: SafeHeaders(headers), NativeError: nativeError, nativeFailure: true}
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

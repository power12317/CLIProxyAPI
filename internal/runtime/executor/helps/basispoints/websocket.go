package basispoints

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// WebsocketDial accepts the native bearer subprotocol separately from log headers.
type WebsocketDial func(context.Context, string, http.Header, []string) (*websocket.Conn, *http.Response, error)

// WebsocketRequest is scoped to one generation, including resume attempts.
// It never determines which credential is selected for a subsequent request.
type WebsocketRequest struct {
	// ObserveConnection reports only physical connection establishment and closure.
	ObserveConnection func(bool)
	URL               string
	Headers           http.Header
	Frame             []byte
	requestID         string
	token             string
	resume            string
}

func NewWebsocketRequest(headers http.Header, body []byte) (*WebsocketRequest, error) {
	u, err := url.Parse(ResponsesURL)
	if err != nil {
		return nil, err
	}
	u.Scheme = "wss"
	clientInfo := make(map[string]string)
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-openai-internal-basispoints-") {
			clientInfo[name] = headers.Get(name)
		}
	}
	info, err := json.Marshal(clientInfo)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Set("bps_client_info", string(info))
	query.Set("bps_auth_mode", "chatgpt")
	query.Set("bps_ws_affinity", "affinity_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	query.Set("bps_control_frames", "upstream_sent,heartbeat,resume,cancel,server_draining")
	u.RawQuery = query.Encode()
	r := &WebsocketRequest{
		URL: u.String(), Headers: headers.Clone(), requestID: uuid.NewString(),
		token:  strings.TrimSpace(strings.TrimPrefix(headers.Get("Authorization"), "Bearer ")),
		resume: "resume_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	r.Headers.Del("Content-Type")
	r.Headers.Del("Accept")
	frame, err := decode(body)
	if err != nil {
		return nil, err
	}
	frame["type"] = "response.create"
	frame["basispoints_request_id"] = r.requestID
	frame["basispoints_resume_token"] = r.resume
	r.Frame, err = json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// FitsMessageLimit follows the original client's 16 MiB outgoing frame limit.
func (r *WebsocketRequest) FitsMessageLimit() bool { return len(r.Frame) <= 16<<20 }

// LogFrame preserves the request shape without exposing the resume capability.
func (r *WebsocketRequest) LogFrame() []byte {
	frame, _ := decode(r.Frame)
	frame["basispoints_resume_token"] = "[REDACTED]"
	raw, _ := json.Marshal(frame)
	return raw
}

// Open reports whether a generation may have been sent. Only handshake failures
// are eligible for the executor's SSE fallback; an in-flight request uses resume.
func (r *WebsocketRequest) Open(ctx context.Context, dial WebsocketDial, observe func([]byte)) (*http.Response, bool, error) {
	conn, response, err := dial(ctx, r.URL, r.Headers.Clone(), []string{"responses", "openai-bearer." + r.token})
	if err != nil {
		return response, false, err
	}
	reader := &websocketReader{ctx: ctx, request: r, dial: dial, observe: observe, conn: conn, resume: r.resume, cursor: -1}
	reader.closeConn = r.connectionCloser(conn)
	conn.SetReadLimit(maxItemBytes)
	reader.stop = context.AfterFunc(ctx, func() { _ = reader.closeConnection() })
	if err = conn.WriteMessage(websocket.TextMessage, r.Frame); err != nil {
		_ = reader.Close()
		return response, true, err
	}
	response.Body = reader
	response.Header = response.Header.Clone()
	response.Header.Set("Content-Type", "text/event-stream")
	return response, true, nil
}

// websocketReader presents native JSON events to the existing SSE protocol bridge.
// No read or write deadlines are added after connection establishment.
type websocketReader struct {
	ctx       context.Context
	request   *WebsocketRequest
	dial      WebsocketDial
	observe   func([]byte)
	stop      func() bool
	mu        sync.Mutex
	conn      *websocket.Conn
	closeConn func() error
	closed    bool
	resume    string
	cursor    int64
	attempts  int
	buffer    bytes.Buffer
	terminal  bool
}

func (r *WebsocketRequest) connectionCloser(conn *websocket.Conn) func() error {
	if r.ObserveConnection != nil {
		r.ObserveConnection(true)
	}
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			err = conn.Close()
			if r.ObserveConnection != nil {
				r.ObserveConnection(false)
			}
		})
		return err
	}
}

func (r *websocketReader) Close() error {
	if r.stop != nil {
		r.stop()
	}
	return r.closeConnection()
}

func (r *websocketReader) closeConnection() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	closeConn := r.closeConn
	r.mu.Unlock()
	return closeConn()
}

func (r *websocketReader) reconnect() error {
	for r.attempts < 3 {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		closed, closeOld := r.closed, r.closeConn
		r.mu.Unlock()
		if closed {
			return io.ErrClosedPipe
		}
		_ = closeOld()
		r.attempts++
		conn, response, err := r.dial(r.ctx, r.request.URL, r.request.Headers.Clone(), []string{"responses", "openai-bearer." + r.request.token})
		if err != nil {
			if response != nil && response.Body != nil {
				raw, errRead := io.ReadAll(io.LimitReader(response.Body, maxItemBytes))
				_ = response.Body.Close()
				if errRead == nil && response.StatusCode >= 400 && response.StatusCode < 500 {
					return &Error{Status: response.StatusCode, Body: string(raw)}
				}
			}
			continue
		}
		conn.SetReadLimit(maxItemBytes)
		closeConn := r.request.connectionCloser(conn)
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = closeConn()
			return io.ErrClosedPipe
		}
		r.conn = conn
		r.closeConn = closeConn
		r.mu.Unlock()
		err = conn.WriteJSON(object{"type": "basispoints.response.resume", "resume_token": r.resume, "after_cursor": r.cursor})
		if err == nil {
			return nil
		}
	}
	return failure(502, "websocket_resume_failed", "Basispoints WebSocket disconnected and could not resume")
}

func (r *websocketReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.buffer.Len() == 0 {
		if r.terminal {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		conn := r.conn
		r.mu.Unlock()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if err = r.reconnect(); err != nil {
				return 0, err
			}
			continue
		}
		if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
			return 0, failure(502, "invalid_stream", "Basispoints WebSocket returned invalid JSON")
		}
		event := gjson.ParseBytes(raw)
		kind := event.Get("type").String()
		if kind == "" {
			return 0, failure(502, "invalid_stream", "Basispoints WebSocket event has no type")
		}
		if strings.HasPrefix(kind, "basispoints.response.") {
			if kind == "basispoints.response.resume_token" && event.Get("request_id").String() == r.request.requestID {
				if token := event.Get("resume_token").String(); token != "" {
					r.resume = token
				}
			} else if r.observe != nil {
				r.observe(raw)
			}
			continue
		}
		if cursor := event.Get("basispoints_replay_cursor"); cursor.Type == gjson.Number {
			if cursor.Int() <= r.cursor {
				continue
			}
			r.cursor = cursor.Int()
		}
		switch kind {
		case "error", "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled":
			r.terminal = true
		}
		if _, err = fmt.Fprintf(&r.buffer, "data: %s\n\n", raw); err != nil {
			return 0, err
		}
	}
	return r.buffer.Read(p)
}

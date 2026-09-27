package basispoints

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// WebsocketActivity uses the existing Codex request-ownership gate.
type WebsocketActivity interface {
	Acquire(context.Context) error
	Release()
}

// WebsocketSession owns a connection across requests and turns. Only one
// request consumes its response stream at a time; idle reads handle peer closure.
type WebsocketSession struct {
	activity WebsocketActivity
	mu       sync.Mutex
	conn     *websocketConnection
	affinity string
	closed   bool
}

func NewWebsocketSession(activity WebsocketActivity) *WebsocketSession {
	return &WebsocketSession{activity: activity}
}

func (r *WebsocketRequest) UseSession(session *WebsocketSession) {
	r.session = session
	u, _ := url.Parse(r.URL)
	query := u.Query()
	session.mu.Lock()
	if session.affinity == "" {
		session.affinity = query.Get("bps_ws_affinity")
	}
	query.Set("bps_ws_affinity", session.affinity)
	session.mu.Unlock()
	u.RawQuery = query.Encode()
	r.URL = u.String()
}

func (s *WebsocketSession) connection(ctx context.Context, request *WebsocketRequest, dial WebsocketDial) (*websocketConnection, *http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if s.closed {
		return nil, nil, io.ErrClosedPipe
	}
	if s.conn != nil && !s.conn.closed.Load() {
		return s.conn, nil, nil
	}
	conn, response, err := dial(ctx, request.URL, request.Headers.Clone(), []string{"responses", "openai-bearer." + request.token})
	if err != nil {
		return nil, response, err
	}
	conn.SetReadLimit(maxItemBytes)
	c := &websocketConnection{conn: conn, headers: response.Header.Clone(), done: make(chan struct{}), observe: request.ObserveConnection}
	if c.observe != nil {
		c.observe(true)
	}
	if err = ctx.Err(); err != nil {
		c.close(err)
		return nil, nil, err
	}
	s.conn = c
	go c.readLoop()
	return c, response, nil
}

func (s *WebsocketSession) Close() {
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.close(io.ErrClosedPipe)
	}
}

type websocketExchange struct {
	frames   chan []byte
	done     chan struct{}
	terminal atomic.Bool
}

type websocketConnection struct {
	conn    *websocket.Conn
	headers http.Header
	observe func(bool)
	once    sync.Once
	closed  atomic.Bool
	done    chan struct{}
	err     error
	writeMu sync.Mutex
	mu      sync.Mutex
	active  *websocketExchange
}

func (c *websocketConnection) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *websocketConnection) attach() (*websocketExchange, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	if c.active != nil {
		return nil, failure(502, "websocket_busy", "Basispoints WebSocket already has an active response")
	}
	exchange := &websocketExchange{frames: make(chan []byte, 32), done: make(chan struct{})}
	c.active = exchange
	return exchange, nil
}

func (c *websocketConnection) detach(exchange *websocketExchange) {
	c.mu.Lock()
	if c.active == exchange {
		c.active = nil
		close(exchange.done)
	}
	c.mu.Unlock()
}

func (c *websocketConnection) close(cause error) {
	c.once.Do(func() {
		c.closed.Store(true)
		c.err = cause
		_ = c.conn.Close()
		if c.observe != nil {
			c.observe(false)
		}
		close(c.done)
	})
}

func (c *websocketConnection) readLoop() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			c.close(err)
			return
		}
		c.mu.Lock()
		exchange := c.active
		c.mu.Unlock()
		if exchange == nil {
			continue
		}
		switch gjson.GetBytes(raw, "type").String() {
		case "error", "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled":
			exchange.terminal.Store(true)
		}
		select {
		case exchange.frames <- raw:
		case <-exchange.done:
		case <-c.done:
			return
		}
	}
}

func (c *websocketConnection) read(ctx context.Context, exchange *websocketExchange) ([]byte, error) {
	// Deliver already received frames before reporting a peer disconnect.
	select {
	case raw := <-exchange.frames:
		return raw, nil
	default:
	}
	select {
	case raw := <-exchange.frames:
		return raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-exchange.done:
		return nil, io.ErrClosedPipe
	case <-c.done:
		select {
		case raw := <-exchange.frames:
			return raw, nil
		default:
			return nil, c.err
		}
	}
}

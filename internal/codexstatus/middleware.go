package codexstatus

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Gateways opt in to status metadata. This header never reaches an executor.
func (m *Monitor) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var values []string
		for name, entries := range c.Request.Header {
			if strings.EqualFold(name, Header) {
				values = append(values, entries...)
				delete(c.Request.Header, name)
			}
		}
		r := m.Request()
		r.negotiated = len(values) == 1 && values[0] == "1"
		c.Request = c.Request.WithContext(WithRequest(c.Request.Context(), r))
		if !r.negotiated {
			c.Next()
			return
		}
		w := &statusWriter{ResponseWriter: c.Writer, request: r, boundary: true}
		c.Writer = w
		c.Next()
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.trailing {
			w.Header().Set(Header, r.Status())
		}
		if w.Written() && w.isStream() {
			if sent, err := w.emit(); sent && err == nil {
				w.ResponseWriter.Flush()
			}
		}
	}
}

type statusWriter struct {
	gin.ResponseWriter
	mu                                    sync.Mutex
	request                               *Request
	boundary, lineContent, sent, trailing bool
}

func (w *statusWriter) isStream() bool {
	return strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
}
func (w *statusWriter) prepare() {
	if w.Written() {
		return
	}
	status := w.request.Status()
	if !w.trailing && !w.isStream() && status != "degraded" {
		// Non-stream keepalive can commit headers before degradation is known.
		w.Header().Add("Trailer", Header)
		w.trailing = true
	}
	if status != "" {
		w.Header().Set(Header, status)
	}
}
func (w *statusWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepare()
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepare()
	w.ResponseWriter.WriteHeaderNow()
}
func (w *statusWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepare()
	if w.isStream() {
		_, _ = w.emit()
	}
	w.ResponseWriter.Flush()
}
func (w *statusWriter) emit() (bool, error) {
	if !w.boundary || w.sent || w.request.Status() != "degraded" {
		return false, nil
	}
	_, err := w.ResponseWriter.WriteString(StreamPrefix + "degraded\n\n")
	if err != nil {
		return false, err
	}
	w.sent = true
	return true, nil
}
func (w *statusWriter) WriteString(value string) (int, error) { return w.Write([]byte(value)) }
func (w *statusWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepare()
	if !w.isStream() {
		return w.ResponseWriter.Write(data)
	}
	written := 0
	for len(data) > 0 {
		if _, err := w.emit(); err != nil {
			return written, err
		}
		end := len(data)
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			end = i + 1
		}
		part := data[:end]
		n, err := w.ResponseWriter.Write(part)
		written += n
		if err != nil {
			return written, err
		}
		if n != len(part) {
			return written, io.ErrShortWrite
		}
		for _, b := range part {
			if b == '\n' {
				w.boundary = !w.lineContent
				w.lineContent = false
			} else if b != '\r' {
				w.lineContent = true
				w.boundary = false
			}
		}
		data = data[end:]
	}
	return written, nil
}

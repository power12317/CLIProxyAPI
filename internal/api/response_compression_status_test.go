package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexstatus"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

const statusCompressionCreated = "data: {\"type\":\"response.created\"}\n\n"
const statusCompressionDelta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\r\n\r\n"
const statusCompressionCompleted = "data: {\"type\":\"response.completed\"}\n\n"
const statusCompressionComment = codexstatus.StreamPrefix + "degraded\n\n"

var statusCompressionEncodings = []struct {
	name, accept, encoding string
}{
	{name: "identity"},
	{name: "gzip", accept: "gzip", encoding: "gzip"},
	{name: "zstd", accept: "zstd", encoding: "zstd"},
	{name: "prefer_zstd", accept: "gzip, zstd", encoding: "zstd"},
}

// Use NewServer's middleware registration, rather than recreating the desired
// order in a test-only router, so reversing the product order breaks this test.
func TestResponseCompressionStatusServerComposition(t *testing.T) {
	for _, encoding := range statusCompressionEncodings {
		for _, negotiated := range []bool{false, true} {
			for _, phase := range []string{"normal", "before_body", "after_flush", "inside_event", "finish"} {
				t.Run(fmt.Sprintf("%s/negotiated=%t/%s", encoding.name, negotiated, phase), func(t *testing.T) {
					server := newTestServer(t)
					t.Cleanup(server.eventMonitor.Close)
					var writer gin.ResponseWriter
					var attempt *codexstatus.Attempt
					server.engine.POST("/status-compression-test", func(c *gin.Context) {
						writer = c.Writer
						if c.GetHeader(codexstatus.Header) != "" {
							t.Error("status negotiation reached the business handler")
						}
						attempt = codexstatus.FromContext(c.Request.Context()).Begin(codexstatus.Credential{ID: "status-fixture"}, "turn", 780)
						if phase == "before_body" {
							attempt.Observe(780)
						}
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeader(http.StatusOK)
						c.Writer.WriteHeaderNow()
						write := func(data string, asString bool) {
							t.Helper()
							var n int
							var err error
							if asString {
								n, err = c.Writer.WriteString(data)
							} else {
								n, err = c.Writer.Write([]byte(data))
							}
							if err != nil || n != len(data) {
								t.Fatalf("write = %d, %v; want %d bytes", n, err, len(data))
							}
						}
						write(statusCompressionCreated, true)
						c.Writer.Flush()
						if phase == "after_flush" {
							attempt.Observe(780)
							// Exercise error-aware flushing before any further business write.
							if err := httpwire.FlushResponse(c.Writer); err != nil {
								t.Fatal(err)
							}
						}
						for i, part := range []string{statusCompressionDelta[:19], statusCompressionDelta[19 : len(statusCompressionDelta)-1], "\n"} {
							write(part, i%2 == 0)
							c.Writer.Flush()
							if phase == "inside_event" && i == 0 {
								attempt.Observe(780)
								c.Writer.Flush()
							}
						}
						write(statusCompressionCompleted, false)
						if err := httpwire.FlushResponse(c.Writer); err != nil {
							t.Fatal(err)
						}
						if phase == "finish" {
							attempt.Observe(780)
						}
					})
					request := httptest.NewRequest(http.MethodPost, "/status-compression-test", nil)
					request.Header.Set("Accept-Encoding", encoding.accept)
					if negotiated {
						request.Header.Set(codexstatus.Header, "1")
					}
					response := httptest.NewRecorder()
					server.engine.ServeHTTP(response, request)
					if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != encoding.encoding {
						t.Fatalf("status/encoding = %d/%q, want 200/%q", response.Code, response.Header().Get("Content-Encoding"), encoding.encoding)
					}
					// Read from byte zero through EOF: gzip validates its trailer and
					// zstd validates the complete frame, including any checksum.
					body := string(compressionTestBody(t, encoding.encoding, response.Body.Bytes()))
					want := statusCompressionCreated + statusCompressionDelta + statusCompressionCompleted
					if negotiated && phase != "normal" {
						switch phase {
						case "before_body":
							want = statusCompressionComment + want
						case "after_flush":
							want = statusCompressionCreated + statusCompressionComment + statusCompressionDelta + statusCompressionCompleted
						case "inside_event":
							want = statusCompressionCreated + statusCompressionDelta + statusCompressionComment + statusCompressionCompleted
						case "finish":
							want += statusCompressionComment
						}
					}
					if body != want || !strings.Contains(body, statusCompressionCompleted) {
						t.Fatalf("decoded SSE = %q, want %q including response.completed", body, want)
					}
					if !response.Flushed {
						t.Fatal("stream was not flushed")
					}
					if !negotiated && response.Header().Get(codexstatus.Header) != "" {
						t.Fatal("status exposed without negotiation")
					}
					if negotiated {
						wantStatus := "normal"
						if phase == "before_body" {
							wantStatus = "degraded"
						}
						if got := response.Result().Header.Get(codexstatus.Header); got != wantStatus {
							t.Fatalf("initial status header = %q, want %q", got, wantStatus)
						}
					}
					if encoding.encoding != "" {
						length := response.Body.Len()
						attempt.Observe(780)
						if _, err := writer.WriteString("data: late\n\n"); !errors.Is(err, io.ErrClosedPipe) {
							t.Fatalf("write after compressor finalization = %v, want closed pipe", err)
						}
						if response.Body.Len() != length {
							t.Fatal("late write bypassed the closed compressor")
						}
					}
				})
			}
		}
	}
}

// Prove that the first complete event is decodable while the handler is still
// blocked, before the compressor is closed or response.completed is produced.
func TestResponseCompressionStatusStreamsBeforeCompletion(t *testing.T) {
	for _, encoding := range statusCompressionEncodings {
		t.Run(encoding.name, func(t *testing.T) {
			server := newTestServer(t)
			t.Cleanup(server.eventMonitor.Close)
			release := make(chan struct{})
			server.engine.POST("/status-compression-live", func(c *gin.Context) {
				attempt := codexstatus.FromContext(c.Request.Context()).Begin(codexstatus.Credential{ID: "live-fixture"}, "turn", 780)
				c.Header("Content-Type", "text/event-stream")
				if _, err := c.Writer.WriteString(statusCompressionCreated); err != nil {
					return
				}
				c.Writer.Flush()
				select {
				case <-release:
				case <-c.Request.Context().Done():
					return
				}
				attempt.Observe(780)
				if _, err := c.Writer.Write([]byte(statusCompressionDelta + statusCompressionCompleted)); err != nil {
					return
				}
				c.Writer.Flush()
			})
			httpServer := httptest.NewServer(server.engine)
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/status-compression-live", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", encoding.accept)
			request.Header.Set(codexstatus.Header, "1")
			client := httpServer.Client()
			client.Transport = &http.Transport{DisableCompression: true}
			defer client.CloseIdleConnections()
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.Header.Get("Content-Encoding") != encoding.encoding || response.Uncompressed {
				t.Fatalf("unexpected transport decoding or encoding: %v", response.Header)
			}
			var decoded io.Reader = response.Body
			switch encoding.encoding {
			case "gzip":
				reader, err := gzip.NewReader(decoded)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = reader.Close() }()
				decoded = reader
			case "zstd":
				reader, err := zstd.NewReader(decoded)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				decoded = reader
			}
			reader := bufio.NewReader(decoded)
			for _, want := range []string{strings.TrimSuffix(statusCompressionCreated, "\n"), "\n"} {
				line, err := reader.ReadString('\n')
				if err != nil || line != want {
					t.Fatalf("initial event before handler release = %q, %v; want %q", line, err, want)
				}
			}
			close(release)
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("complete compressed stream: %v", err)
			}
			if string(body) != statusCompressionComment+statusCompressionDelta+statusCompressionCompleted {
				t.Fatalf("final events = %q", body)
			}
		})
	}
}

func TestResponseCompressionStatusErrorFinalization(t *testing.T) {
	for _, encoding := range statusCompressionEncodings {
		for _, ending := range []string{"cancel", "sse_error", "panic"} {
			t.Run(encoding.name+"/"+ending, func(t *testing.T) {
				server := newTestServer(t)
				t.Cleanup(server.eventMonitor.Close)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				const errorEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"upstream failed\"}}\n\n"
				server.engine.POST("/status-compression-error", func(c *gin.Context) {
					attempt := codexstatus.FromContext(c.Request.Context()).Begin(codexstatus.Credential{ID: "error-fixture"}, "turn", 780)
					c.Header("Content-Type", "text/event-stream")
					if _, err := c.Writer.WriteString(statusCompressionCreated); err != nil {
						t.Fatal(err)
					}
					c.Writer.Flush()
					attempt.Observe(780)
					if ending == "panic" {
						c.Writer.Flush()
						panic("status compression test panic")
					}
					if ending == "cancel" {
						cancel()
					} else if _, err := c.Writer.Write([]byte(errorEvent)); err != nil {
						t.Fatal(err)
					}
					c.Abort()
				})
				request := httptest.NewRequest(http.MethodPost, "/status-compression-error", nil).WithContext(ctx)
				request.Header.Set("Accept-Encoding", encoding.accept)
				request.Header.Set(codexstatus.Header, "1")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Header().Get("Content-Encoding") != encoding.encoding {
					t.Fatalf("encoding = %q", response.Header().Get("Content-Encoding"))
				}
				body := string(compressionTestBody(t, encoding.encoding, response.Body.Bytes()))
				want := statusCompressionCreated + statusCompressionComment
				if ending == "sse_error" {
					want += errorEvent
				}
				if body != want || strings.Contains(body, statusCompressionCompleted) {
					t.Fatalf("interrupted stream = %q, want %q without a success completion", body, want)
				}
			})
		}
	}
}

type statusCompressionFailingRecorder struct {
	*httptest.ResponseRecorder
	fail bool
}

func (w *statusCompressionFailingRecorder) Write(data []byte) (int, error) {
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(data)
}

func (w *statusCompressionFailingRecorder) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *statusCompressionFailingRecorder) FlushError() error {
	if w.fail {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestResponseCompressionStatusTransportFailure(t *testing.T) {
	for _, encoding := range statusCompressionEncodings {
		t.Run(encoding.name, func(t *testing.T) {
			server := newTestServer(t)
			t.Cleanup(server.eventMonitor.Close)
			response := &statusCompressionFailingRecorder{ResponseRecorder: httptest.NewRecorder()}
			var prefix []byte
			server.engine.POST("/status-compression-failure", func(c *gin.Context) {
				attempt := codexstatus.FromContext(c.Request.Context()).Begin(codexstatus.Credential{ID: "transport-fixture"}, "turn", 780)
				c.Header("Content-Type", "text/event-stream")
				if _, err := c.Writer.WriteString(statusCompressionCreated); err != nil {
					t.Fatal(err)
				}
				c.Writer.Flush()
				prefix = bytes.Clone(response.Body.Bytes())
				response.fail = true
				attempt.Observe(780)
				if _, err := c.Writer.Write([]byte(statusCompressionDelta)); !errors.Is(err, io.ErrClosedPipe) {
					// A codec may buffer this write; flushing must surface the failure.
					if err != nil {
						t.Fatal(err)
					}
				}
				c.Writer.Flush()
				if _, err := c.Writer.WriteString(statusCompressionCompleted); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("write after transport failure = %v, want closed pipe", err)
				}
			})
			request := httptest.NewRequest(http.MethodPost, "/status-compression-failure", nil)
			request.Header.Set("Accept-Encoding", encoding.accept)
			request.Header.Set(codexstatus.Header, "1")
			server.engine.ServeHTTP(response, request)
			if !bytes.Equal(response.Body.Bytes(), prefix) {
				t.Fatal("error finalization appended data after the transport stopped accepting encoded writes")
			}
		})
	}
}

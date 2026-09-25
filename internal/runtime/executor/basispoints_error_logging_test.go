package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestBasispointsFullErrorFiles(t *testing.T) {
	for _, requestLog := range []bool{false, true} {
		for _, mode := range []string{"rejected", "invalid_tool_json", "invalid_tool_stream", "success"} {
			t.Run(fmt.Sprintf("request_log=%v/%s", requestLog, mode), func(t *testing.T) {
				logsDir := t.TempDir()
				cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
				cfg.RequestLog = requestLog
				logger := logging.NewFileRequestLogger(requestLog, logsDir, "", 10)
				original := []byte(`{ "model":"gpt-6-astra", "input":"complete client request: start/end", "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}] }`)
				badCode := "unparseable tool code: " + strings.Repeat("x", 188)
				args, _ := json.Marshal(map[string]any{"summary": "Shell", "code": badCode})
				upstreamPayload, _ := json.Marshal(map[string]any{"id": "r", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_error", "call_id": "call_error", "name": "run_officejs", "arguments": string(args)}}})
				if mode == "success" {
					upstreamPayload = []byte(`{"id":"r","status":"completed","output":[]}`)
				}
				if mode == "rejected" {
					upstreamPayload = []byte(`{"error":{"message":"422: Invalid request body."}}`)
				}
				var sent []byte
				calls := 0
				router := gin.New()
				router.Use(logging.GinLogrusLogger(), middleware.RequestLoggingMiddleware(logger))
				router.POST("/v1/responses", func(c *gin.Context) {
					body, err := io.ReadAll(c.Request.Body)
					if err != nil {
						t.Error(err)
						c.Status(500)
						return
					}
					ctx := context.WithValue(c.Request.Context(), "gin", c)
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						sent, _ = io.ReadAll(r.Body)
						status, contentType, payload := 200, "application/json", string(upstreamPayload)
						if mode == "rejected" {
							status = 422
						}
						if mode == "invalid_tool_stream" {
							contentType = "text/event-stream"
							payload = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" + "data: {\"type\":\"response.completed\",\"response\":" + string(upstreamPayload) + "}\n\n"
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
					})))
					auth := &coreauth.Auth{ID: t.Name(), FileName: "diagnostic-auth.json", Provider: "codex", Metadata: map[string]any{"access_token": "upstream-secret-key", "account_id": "account"}}
					exec := NewBasispointsExecutor(cfg)
					req := coreexecutor.Request{Model: "gpt-6-astra", Payload: body}
					opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body}
					if mode == "invalid_tool_stream" {
						result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
						if errStream != nil {
							t.Error(errStream)
							c.Status(500)
							return
						}
						c.Header("Content-Type", "text/event-stream")
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								_, _ = c.Writer.Write([]byte("data: " + chunk.Err.Error() + "\n\n"))
								continue
							}
							_, _ = c.Writer.Write(chunk.Payload)
						}
					} else {
						result, errExecute := exec.Execute(ctx, auth, req, opts)
						if errExecute != nil {
							c.Data(errExecute.(interface{ StatusCode() int }).StatusCode(), "application/json", []byte(errExecute.Error()))
							return
						}
						c.Data(200, "application/json", result.Payload)
					}
				})
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer client-secret-key")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if calls != 1 {
					t.Fatalf("unexpected automatic retry: calls=%d", calls)
				}
				files, err := filepath.Glob(filepath.Join(logsDir, "*.log"))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "success" && !requestLog {
					if len(files) != 0 {
						t.Fatal("successful request created error log")
					}
					return
				}
				if len(files) != 1 {
					t.Fatalf("want one log file, got %v", files)
				}
				if !requestLog && !strings.HasPrefix(filepath.Base(files[0]), "error-") {
					t.Fatal("missing error log prefix")
				}
				content, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, expected := range [][]byte{original, sent, upstreamPayload} {
					if !bytes.Contains(content, expected) {
						t.Fatalf("log lacks complete section of %d bytes", len(expected))
					}
				}
				if strings.HasPrefix(mode, "invalid_tool") && (!bytes.Contains(content, []byte("Transport code:\n"+badCode)) || !bytes.Contains(content, []byte("stage=transport_code"))) {
					t.Fatal("malformed tool input was not logged completely")
				}
				for _, secret := range []string{"client-secret-key", "upstream-secret-key"} {
					if bytes.Contains(content, []byte(secret)) {
						t.Fatal("authorization header leaked")
					}
				}
				if mode == "invalid_tool_stream" && response.Code != 200 {
					t.Fatal("stream status changed instead of logging its error")
				}
			})
		}
	}
}

func TestBasispointsErrorFilesDoNotTruncateLargeBodies(t *testing.T) {
	dir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, dir, "", 10)
	body := append([]byte(`{"input":"`), bytes.Repeat([]byte("x"), (32<<20)+128)...)
	body = append(body, []byte(`-CLIENT-BODY-END"}`)...)
	wire := bytes.Replace(body, []byte("CLIENT-BODY-END"), []byte("UPSTREAM-BODY-END"), 1)
	router := gin.New()
	router.Use(middleware.RequestLoggingMiddleware(logger))
	router.POST("/v1/responses", func(c *gin.Context) {
		original, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Error(err)
			c.Status(500)
			return
		}
		ctx := context.WithValue(c.Request.Context(), "gin", c)
		helps.RecordBasispointsFailure(ctx, &config.Config{}, original, wire, []byte(`{"error":"bad input"}`), "upstream_rejected", fmt.Errorf("bad input"))
		c.Status(422)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
	files, _ := filepath.Glob(filepath.Join(dir, "error-*.log"))
	if len(files) != 1 {
		t.Fatalf("missing error file: %v", files)
	}
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, body) || !bytes.Contains(content, wire) || bytes.Contains(content, []byte("BODY TRUNCATED")) {
		t.Fatal("error request bodies were truncated")
	}
}

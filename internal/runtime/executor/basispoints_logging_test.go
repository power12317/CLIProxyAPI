package executor

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestBasispointsGinLogsUseCodexFieldsOnSuccessAndRejection(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })
	for _, stream := range []bool{false, true} {
		for _, status := range []int{200, 422} {
			t.Run(fmt.Sprintf("stream=%v/status=%d", stream, status), func(t *testing.T) {
				hook.Reset()
				cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
				auth := &coreauth.Auth{ID: t.Name(), FileName: "/auths/basispoints-test.json", Provider: "codex", Metadata: map[string]any{"access_token": "test-secret-token", "account_id": "account"}}
				t.Cleanup(func() { helps.InvalidateCodexTurnStates(auth.ID) })
				body := []byte(`{"model":"gpt-6-astra","input":"hello","client_metadata":{"session_id":"11111111-session","turn_id":"22222222-turn"}}`)
				server := gin.New()
				server.Use(logging.GinLogrusLogger())
				server.POST("/v1/responses", func(c *gin.Context) {
					ctx := context.WithValue(c.Request.Context(), "gin", c)
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						raw, _ := io.ReadAll(r.Body)
						if r.Header.Get("X-Codex-Turn-State") != "" || gjson.GetBytes(raw, "client_metadata").Exists() {
							t.Fatal("logging changed the Basispoints wire request")
						}
						payload := fmt.Sprintf("data: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":%q}}\n\n", strings.Repeat("y", 312)) + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"resolved-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
						if status == 422 {
							payload = `{"error":{"message":"422: Invalid request body."}}`
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {strings.Repeat("x", 780)}, "X-Request-Id": {"upstream-test-id"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
					})))
					executor := NewBasispointsExecutor(cfg)
					req := coreexecutor.Request{Model: "gpt-6-astra", Payload: body}
					opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
					var err error
					if stream {
						var result *coreexecutor.StreamResult
						result, err = executor.ExecuteStream(ctx, auth, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Error(chunk.Err)
								}
								_, _ = c.Writer.Write(chunk.Payload)
							}
						}
					} else {
						_, err = executor.Execute(ctx, auth, req, opts)
					}
					if err != nil {
						c.Status(err.(interface{ StatusCode() int }).StatusCode())
					} else {
						c.Status(200)
					}
				})
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))))
				if recorder.Code != status {
					t.Fatalf("status=%d, want %d", recorder.Code, status)
				}
				var access *log.Entry
				for _, entry := range hook.AllEntries() {
					if strings.Contains(entry.Message, `POST`) && strings.Contains(entry.Message, `"/v1/responses"`) {
						access = entry
					}
				}
				if access == nil {
					t.Fatal("missing Gin access log")
				}
				for key, want := range map[string]string{"auth_file": "basispoints-test.json", "session_id": "11111111", "turn_id": "22222222"} {
					if access.Data[key] != want {
						t.Fatalf("%s=%v, want %s", key, access.Data[key], want)
					}
				}
				id, _ := access.Data["request_id"].(string)
				if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 4 {
					t.Fatalf("invalid request ID: %q", id)
				}
				wantLength, wantModel := "0/312", "gpt-6-astra/resolved-model"
				if status == 422 {
					wantLength, wantModel = "0/780", "gpt-6-astra/"
				}
				formatted, _ := (&logging.LogFormatter{}).Format(access)
				if !strings.Contains(string(formatted), wantLength) || !strings.Contains(string(formatted), wantModel) || strings.Contains(string(formatted), "test-secret-token") {
					t.Fatalf("Codex log fields not aligned: %s", formatted)
				}
			})
		}
	}
}

func TestBasispointsHostedToolsUseNativeCodexWithoutRemovingDeclarations(t *testing.T) {
	for _, kind := range []string{"web_search", "image_generation", "tool_search"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", kind, stream), func(t *testing.T) {
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}, Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
				executor := NewCodexAutoExecutor(cfg)
				auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					raw, _ := io.ReadAll(r.Body)
					if r.URL.Host != "chatgpt.com" || gjson.GetBytes(raw, "tools.0.type").String() != kind {
						t.Fatalf("hosted tool removed or routed to wrong host: %s %s", r.URL, raw)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
				})))
				req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":"hello","tools":[{"type":%q}]}`, kind))}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("upstream calls=%d", calls)
				}
			})
		}
	}
}

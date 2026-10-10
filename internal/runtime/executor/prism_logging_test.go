package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func prismCapturedLog(t *testing.T, ctx *gin.Context, key string) string {
	t.Helper()
	value, exists := ctx.Get(key)
	data, ok := value.([]byte)
	if !exists || !ok {
		t.Fatalf("missing %s log", key)
	}
	return string(data)
}

func TestPrismRequestLogsJSONAndSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		body := prismResponse("gpt-6.1-sol")
		if gjson.GetBytes(payload, "stream").Bool() {
			body = helps.PrismEvents(body)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	for _, stream := range []bool{false, true} {
		cfg := prismConfig(t, server.URL)
		cfg.RequestLog = true
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx := context.WithValue(t.Context(), "gin", ginCtx)
		req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"private-prompt","reasoning":{"effort":"xhigh"}}`)}
		executor := NewPrismExecutor(cfg)
		if stream {
			result, err := executor.ExecuteStream(ctx, prismTestAuth(), req, prismOptions())
			if err != nil {
				t.Fatal(err)
			}
			for range result.Chunks {
			}
		} else if _, err := executor.Execute(ctx, prismTestAuth(), req, prismOptions()); err != nil {
			t.Fatal(err)
		}
		requestLog := prismCapturedLog(t, ginCtx, "API_REQUEST")
		responseLog := prismCapturedLog(t, ginCtx, "API_RESPONSE")
		if !strings.Contains(requestLog, server.URL+"/v1/responses") || !strings.Contains(requestLog, `"reasoning_effort":"xhigh"`) || !strings.Contains(responseLog, "Status: 200") {
			t.Fatalf("stream=%t missing request/response logging: %s %s", stream, requestLog, responseLog)
		}
		for _, secret := range []string{"synthetic-oauth", "private-prompt", `"text":"answer"`} {
			if strings.Contains(requestLog+responseLog, secret) {
				t.Fatalf("business data appeared in diagnostic logs: %s", secret)
			}
		}
	}
}

func TestPrismLogsFinalToolDescriptionAfterPayloadRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		description := gjson.GetBytes(payload, "tools.0.description")
		if !description.Exists() || description.Type != gjson.Null {
			t.Error("configured description did not reach the adapter")
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_tools","message":"Tool description is invalid"}}`))
	}))
	defer server.Close()
	cfg := prismConfig(t, server.URL)
	cfg.RequestLog = true
	cfg.Payload.Override = []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-6.1-sol", Protocol: "codex"}},
		Params: map[string]any{"tools.0.description": nil},
	}}
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(t.Context(), "gin", ginCtx)
	req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hi","tools":[{"type":"function","name":"lookup","description":"original-description"}]}`)}
	_, err := NewPrismExecutor(cfg).Execute(ctx, prismTestAuth(), req, prismOptions())
	if err == nil || !strings.Contains(err.Error(), "Tool description is invalid") {
		t.Fatalf("adapter error changed: %v", err)
	}
	responseLog := prismCapturedLog(t, ginCtx, "API_RESPONSE")
	for _, want := range []string{"Status: 422", `"error_type":"invalid_tools"`, `"tool_name":"lookup"`, `"description_type":"null"`, `"tool_path":"tools.0.description"`} {
		if !strings.Contains(responseLog, want) {
			t.Errorf("missing %s in %s", want, responseLog)
		}
	}
}

func TestPrismLogsConnectionAndReadFailuresWithoutReplay(t *testing.T) {
	for _, mode := range []string{"disconnect", "truncated response"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "truncated response" {
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("private-partial-response"))
					return
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				if errClose := connection.Close(); errClose != nil {
					t.Error(errClose)
				}
			}))
			defer server.Close()
			cfg := prismConfig(t, server.URL)
			cfg.RequestLog = true
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := context.WithValue(t.Context(), "gin", ginCtx)
			req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hi"}`)}
			_, err := NewPrismExecutor(cfg).Execute(ctx, prismTestAuth(), req, prismOptions())
			stop, ok := err.(interface{ IsRequestStop() bool })
			if !ok || !stop.IsRequestStop() || calls.Load() != 1 {
				t.Fatalf("failure was lost or replayed: err=%v calls=%d", err, calls.Load())
			}
			want := "transport: EOF"
			if mode == "truncated response" {
				want = "read_response: unexpected EOF"
			}
			responseLog := prismCapturedLog(t, ginCtx, "API_RESPONSE")
			if !strings.Contains(responseLog, want) || strings.Contains(responseLog, "private-partial-response") {
				t.Fatalf("response diagnostic=%s", responseLog)
			}
		})
	}
}

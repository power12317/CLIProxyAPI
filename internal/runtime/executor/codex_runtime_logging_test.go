package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func runtimeLogNotification(c *websocket.Conn, req bridge.Request, entry bridge.UpstreamLog) {
	entry.RequestID = req.RequestID
	_ = c.WriteJSON(map[string]any{"method": "cpa/inference/upstream", "params": entry})
}

func runtimeLogRequest() bridge.UpstreamLog {
	return bridge.UpstreamLog{Kind: "request", URL: "https://chatgpt.com/backend-api/codex/responses", Method: "POST", Headers: http.Header{
		"content-type": {"application/json"}, "Authorization": {"Bearer runtime-secret"},
		"X-Codex-Turn-Metadata": {`{"turn_id":"turn-actual","session_id":"session-actual"}`},
		"X-Codex-Turn-State":    {"request-state"},
	}, Body: `{"model":"runtime-model","input":[],"instructions":"actual worker body"}`}
}

func runtimeLoggingContext() (context.Context, *gin.Context) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ctx := logging.WithResponseHeadersHolder(context.WithValue(c.Request.Context(), "gin", c))
	return ctx, c
}

func runtimeLogText(t *testing.T, c *gin.Context, key string) string {
	t.Helper()
	v, ok := c.Get(key)
	if !ok {
		t.Fatalf("missing %s", key)
	}
	return string(v.([]byte))
}

func TestCodexRuntimeUpstreamLogsForSuccessAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			w, a := runtimeTestWorker(t, "logs", func(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
				runtimeLogNotification(c, req, runtimeLogRequest())
				runtimeLogNotification(c, req, bridge.UpstreamLog{Kind: "response", StatusCode: 200, Headers: http.Header{
					"x-request-id": {"actual-request-id"}, "x-codex-turn-state": {"response-state"}, "x-codex-primary-used-percent": {"17"},
				}, OaiLBNode: "gateway-a"})
				runtimeTestAccept(c, m, req)
				runtimeTestFinish(c, req, `{"type":"response.future_extension","future":"retained"}`, runtimeTerminal)
			})
			a.FileName = "shared-account.json"
			e := runtimeTestExecutor(w)
			e.cfg.RequestLog = true
			ctx, c := runtimeLoggingContext()
			req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}
			if stream {
				result, err := e.ExecuteStream(ctx, a, req, runtimeTestOptions())
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(ctx, a, req, runtimeTestOptions()); err != nil {
				t.Fatal(err)
			}
			requestLog := runtimeLogText(t, c, "API_REQUEST")
			responseLog := runtimeLogText(t, c, "API_RESPONSE")
			for _, want := range []string{"=== API REQUEST 1 ===", "HTTP Method: POST", "https://chatgpt.com/backend-api/codex/responses", "actual worker body", "provider=codex"} {
				if !strings.Contains(requestLog, want) {
					t.Errorf("request log missing %q: %s", want, requestLog)
				}
			}
			if strings.Contains(requestLog, "runtime-secret") {
				t.Error("request logs bypassed existing token masking")
			}
			for _, want := range []string{"Status: 200", "actual-request-id", "response-state", "future_extension", "response.completed", "oailb_node: gateway-a", "auth_file: \"shared-account.json\""} {
				if !strings.Contains(responseLog, want) {
					t.Errorf("response log missing %q: %s", want, responseLog)
				}
			}
			fields, ok := logging.CodexTurnStateLogFieldsForContext(c)
			if !ok || fields.AuthFile != a.FileName || fields.SessionID != "session-actual" || fields.TurnID != "turn-actual" || fields.RequestedModel != "runtime-model" || fields.ResponseModel != "runtime-model" || fields.RequestTurnStateLen != len("request-state") || fields.ResponseTurnStateLen != len("response-state") {
				t.Errorf("access-log fields lost: %+v", fields)
			}
			if got := logging.GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent"); got != "17" {
				t.Errorf("monitoring response metadata = %q", got)
			}
		})
	}
}

func TestCodexRuntimeLogsRetriesAndUpstreamRejection(t *testing.T) {
	const failure = `{"error":{"type":"rate_limit_error","message":"account quota exhausted"}}`
	w, a := runtimeTestWorker(t, "reject", func(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
		runtimeLogNotification(c, req, runtimeLogRequest())
		runtimeLogNotification(c, req, bridge.UpstreamLog{Kind: "response", StatusCode: 401, Body: "expired token"})
		runtimeLogNotification(c, req, runtimeLogRequest())
		runtimeLogNotification(c, req, bridge.UpstreamLog{Kind: "response", StatusCode: 429, Headers: http.Header{"retry-after": {"37"}, "x-request-id": {"rejected-request"}}, Body: failure})
		_ = c.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "request rejected", "data": map[string]any{"httpStatus": 429}}})
	})
	e := runtimeTestExecutor(w)
	e.cfg.RequestLog = true
	ctx, c := runtimeLoggingContext()
	_, err := e.Execute(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"input":[]}`)}, runtimeTestOptions())
	var remote *bridge.Error
	if !errors.As(err, &remote) || remote.StatusCode() != 429 || string(remote.ResponseBody()) != failure || remote.Headers().Get("Retry-After") != "37" {
		t.Fatalf("upstream error details lost: %#v", err)
	}
	requests := runtimeLogText(t, c, "API_REQUEST")
	responses := runtimeLogText(t, c, "API_RESPONSE")
	if !strings.Contains(requests, "=== API REQUEST 2 ===") || strings.Contains(requests, "=== API REQUEST 3 ===") {
		t.Fatal("actual retry count missing", requests)
	}
	for _, want := range []string{"Status: 401", "expired token", "Status: 429", "rejected-request", failure} {
		if !strings.Contains(responses, want) {
			t.Errorf("response log missing %q: %s", want, responses)
		}
	}
}

func TestCodexRuntimeLogsStreamFailureAndDisconnect(t *testing.T) {
	for _, mode := range []string{"failed-event", "disconnect", "transport-error"} {
		t.Run(mode, func(t *testing.T) {
			w, a := runtimeTestWorker(t, mode, func(c *websocket.Conn, m runtimeTestRPC, req bridge.Request) {
				runtimeLogNotification(c, req, runtimeLogRequest())
				if mode == "transport-error" {
					runtimeLogNotification(c, req, bridge.UpstreamLog{Kind: "error", Message: "upstream connection reset"})
					_ = c.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "upstream connection reset"}})
					return
				}
				runtimeLogNotification(c, req, bridge.UpstreamLog{Kind: "response", StatusCode: 200})
				runtimeTestAccept(c, m, req)
				if mode == "failed-event" {
					runtimeTestFinish(c, req, `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"out of capacity"}}}`)
				}
			})
			e := runtimeTestExecutor(w)
			e.cfg.RequestLog = true
			ctx, c := runtimeLoggingContext()
			result, err := e.ExecuteStream(ctx, a, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{}`)}, runtimeTestOptions())
			if err == nil {
				for range result.Chunks {
				}
			}
			responseLog := runtimeLogText(t, c, "API_RESPONSE")
			want := "connection closed before completion"
			if mode == "failed-event" {
				want = "out of capacity"
			} else if mode == "transport-error" {
				want = "upstream connection reset"
			}
			if !strings.Contains(responseLog, "Error:") || !strings.Contains(responseLog, want) {
				t.Fatalf("failure log missing: %s", responseLog)
			}
		})
	}
}

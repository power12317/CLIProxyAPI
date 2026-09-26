package logging

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestIsAIAPIPathIncludesPublicAPIGroups(t *testing.T) {
	for _, path := range []string{
		"/v1",
		"/v1/models",
		"/v1/alpha/search",
		"/v1beta/interactions",
		"/openai/v1/videos",
		"/backend-api/codex/responses",
	} {
		if !isAIAPIPath(path) {
			t.Fatalf("expected %s to be treated as AI API path", path)
		}
	}
	for _, path := range []string{
		"/v0/management/config",
		"/v10/models",
		"/openai/v10/videos",
		"/backend-api/codex-status",
	} {
		if isAIAPIPath(path) {
			t.Fatalf("expected %s not to be treated as AI API path", path)
		}
	}
}

func TestIsAIAPIPathIncludesImages(t *testing.T) {
	if !isAIAPIPath("/v1/images/generations") {
		t.Fatalf("expected /v1/images/generations to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/images/edits") {
		t.Fatalf("expected /v1/images/edits to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos") {
		t.Fatalf("expected /v1/videos to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos/video_123") {
		t.Fatalf("expected /v1/videos/video_123 to be treated as AI API path")
	}
	if !isAIAPIPath("/openai/v1/videos") {
		t.Fatalf("expected /openai/v1/videos to be treated as AI API path")
	}
	if !isAIAPIPath("/openai/v1/videos/video_123/content") {
		t.Fatalf("expected /openai/v1/videos/video_123/content to be treated as AI API path")
	}
}

func TestIsAIAPIPathIncludesCodexBackend(t *testing.T) {
	paths := []string{
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/compact",
	}
	for _, path := range paths {
		if !isAIAPIPath(path) {
			t.Fatalf("expected %s to be treated as AI API path", path)
		}
	}
	if isAIAPIPath("/backend-api/codex-status") {
		t.Fatalf("expected /backend-api/codex-status not to be treated as AI API path")
	}
}

func TestGinLogrusLoggerAddsRequestIDForCodexBackend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	var requestIDFromContext string
	var requestIDFromGin string
	engine.POST("/backend-api/codex/responses", func(c *gin.Context) {
		requestIDFromContext = GetRequestID(c.Request.Context())
		requestIDFromGin = GetGinRequestID(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if requestIDFromContext == "" {
		t.Fatalf("expected request ID in request context")
	}
	if requestIDFromGin != requestIDFromContext {
		t.Fatalf("expected Gin request ID %q to match context request ID %q", requestIDFromGin, requestIDFromContext)
	}
}

func TestGinLogrusLoggerAppendsCodexTurnStateFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.POST("/v1/responses", func(c *gin.Context) {
		SetCodexTurnStateLogFields(c, CodexTurnStateLogFields{
			AuthFile:             "codex-user-windows.json",
			SessionID:            "session-1",
			TurnID:               "turn-1",
			RequestedModel:       "gpt-6-astra",
			ResponseModel:        "gpt-5.6-luna",
			RequestTurnStateLen:  7,
			ResponseTurnStateLen: 42,
		})
		SetCodexOaiLBNode(c, "unified-96")
		c.Status(http.StatusBadRequest)
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	message := entries[0].Message
	for _, want := range []string{
		`POST    "/v1/responses"`,
		" | unified-96 | gpt-6-astra/gpt-5.6-luna | 7/42 | ",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("access log = %q, missing %q", message, want)
		}
	}
	columns := strings.Split(message, " | ")
	if len(columns) != 7 || columns[0] != "400" || columns[1] != strings.TrimSpace(columns[1]) {
		t.Fatalf("unexpected access-log column layout or padded duration: %q", message)
	}
	if strings.Contains(message, "oailb_node=") {
		t.Fatalf("access log contains obsolete node suffix: %q", message)
	}
	if strings.Contains(message, "codex upstream response") || strings.Contains(message, "codex turn state") {
		t.Fatalf("unexpected separate turn-state log message in access log = %q", message)
	}
	if strings.Contains(message, "{") || strings.Contains(message, "}") {
		t.Fatalf("access log contains forbidden braces = %q", message)
	}
	if requestID, ok := entries[0].Data["request_id"].(string); !ok || requestID == "" {
		t.Fatalf("request_id = %v, want non-empty request ID", entries[0].Data["request_id"])
	}
	if got := entries[0].Data["session_id"]; got != "session" {
		t.Fatalf("session_id log field = %v, want first identifier segment", got)
	}
	if got := entries[0].Data["turn_id"]; got != "turn" {
		t.Fatalf("turn_id log field = %v, want first identifier segment", got)
	}
}

func TestShortCodexIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "uuid", in: "01a0b731-53d0-70e2-856a-378239388662", want: "01a0b731"},
		{name: "short non uuid", in: "session", want: "session"},
		{name: "long non uuid", in: "abcdefghijk", want: "abcdefgh"},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortCodexIdentifier(tc.in); got != tc.want {
				t.Fatalf("ShortCodexIdentifier(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGinLogrusLoggerPrintsBothTurnStateLengths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	for _, tc := range []struct {
		name        string
		requestLen  int
		responseLen int
		want        string
	}{
		{name: "response only", requestLen: 0, responseLen: 292, want: "0/292"},
		{name: "both changed", requestLen: 292, responseLen: 312, want: "292/312"},
		{name: "request only", requestLen: 292, responseLen: 0, want: "292/0"},
		{name: "request refreshed", requestLen: 312, responseLen: 0, want: "312/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			engine := gin.New()
			engine.Use(GinLogrusLogger())
			engine.POST("/v1/responses", func(c *gin.Context) {
				SetCodexTurnStateLogFields(c, CodexTurnStateLogFields{
					RequestedModel:       "gpt-6-astra",
					ResponseModel:        "gpt-5.6-luna",
					RequestTurnStateLen:  tc.requestLen,
					ResponseTurnStateLen: tc.responseLen,
				})
				c.Status(http.StatusOK)
			})
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			entries := hook.AllEntries()
			if len(entries) != 1 || !strings.Contains(entries[0].Message, tc.want) {
				t.Fatalf("access log entries = %#v, want length %q", entries, tc.want)
			}
			if !strings.Contains(entries[0].Message, " | - | gpt-6-astra/gpt-5.6-luna | ") {
				t.Fatalf("access log missing unknown-node placeholder: %q", entries[0].Message)
			}
		})
	}
}

func TestGinLogrusLoggerHealthProbeStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })
	for _, tc := range []struct {
		name, method, path string
		status             int
		wantLog            bool
	}{
		{"get_ok", "GET", "/healthz", 200, false},
		{"head_ok", "HEAD", "/healthz", 200, false},
		{"success_boundary", "GET", "/healthz", 299, false},
		{"redirect", "GET", "/healthz", 300, true},
		{"client_error", "GET", "/healthz", 400, true},
		{"server_error", "HEAD", "/healthz", 503, true},
		{"similar_path", "GET", "/healthz-extra", 200, true},
		{"other_method", "POST", "/healthz", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(GinLogrusLogger())
			// Simulate an early global middleware response before route handlers run.
			engine.Use(func(c *gin.Context) { c.AbortWithStatus(tc.status) })
			engine.Handle(tc.method, tc.path, func(c *gin.Context) { t.Error("aborted request reached route handler") })
			hook.Reset()
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			if got := len(hook.AllEntries()) > 0; got != tc.wantLog {
				t.Errorf("access log present = %v, want %v", got, tc.wantLog)
			}
		})
	}
}

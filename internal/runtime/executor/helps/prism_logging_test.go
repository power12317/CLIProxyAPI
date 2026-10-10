package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestPrismDescriptionDiagnosticsFollowAdapterSemantics(t *testing.T) {
	for _, tt := range []struct {
		name, kind string
		value      any
		chars      int
	}{
		{name: "missing"},
		{name: "empty", value: ""},
		{name: "Unicode boundary", value: strings.Repeat("界", 32000)},
		{name: "null", kind: "null", value: nil},
		{name: "object", kind: "object", value: map[string]any{}},
		{name: "array", kind: "array", value: []any{}},
		{name: "boolean", kind: "boolean", value: false},
		{name: "number", kind: "number", value: 7},
		{name: "Unicode too long", kind: "string", value: strings.Repeat("😀", 32001), chars: 32001},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tool := map[string]any{"type": "function", "name": "lookup"}
			if tt.name != "missing" {
				tool["description"] = tt.value
			}
			for _, source := range []string{"tools", "additional_tools", "input"} {
				tools := []any{map[string]any{"type": "namespace", "name": "client", "tools": []any{tool}}}
				body := map[string]any{source: tools}
				path := source + ".0.tools.0.description"
				if source == "input" {
					body[source] = []any{map[string]any{"type": "additional_tools", "tools": tools}}
					path = "input.0.tools.0.tools.0.description"
				}
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				original := string(payload)
				fields := prismInvalidToolDescription(payload)
				if string(payload) != original {
					t.Fatal("diagnostics mutated the request")
				}
				if tt.kind == "" {
					if fields != nil {
						t.Fatalf("accepted description diagnosed as invalid: %v", fields)
					}
					continue
				}
				if fields["description_type"] != tt.kind || fields["tool_name"] != "client.lookup" || fields["tool_path"] != path {
					t.Fatalf("source=%s diagnostic=%v", source, fields)
				}
				if tt.chars > 0 && (fields["description_chars"] != tt.chars || fields["description_issue"] != "too_long") {
					t.Fatalf("Unicode length diagnostic=%v", fields)
				}
			}
		})
	}
}

func TestPrismTransportDiagnosticsPreserveCausesWithoutReflectedSecrets(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, syscall.ECONNREFUSED.Error()},
		{fmt.Errorf("wrapped: %w", context.Canceled), "context canceled"},
		{context.DeadlineExceeded, "context deadline exceeded"},
		{io.EOF, "EOF"},
		{io.ErrUnexpectedEOF, "unexpected EOF"},
		{errors.New("invalid header value: secret-token"), "invalid HTTP header"},
		{errors.New("malformed response: secret-token"), "transport error (*errors.errorString)"},
	} {
		err := &url.Error{Op: "Post", URL: "http://127.0.0.1:8319/v1/responses", Err: tt.err}
		if got := prismTransportError(err); got != tt.want || strings.Contains(got, "secret-token") {
			t.Fatalf("diagnostic=%q, want %q", got, tt.want)
		}
	}
}

func TestPrismDiagnosticsReachConsoleWithoutRequestLogAndOmitBodies(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	logger.SetLevel(log.InfoLevel)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(logging.WithRequestID(t.Context(), "request-fixture"), "gin", ginCtx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8319/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Prism-OAuth-Token", "secret-oauth")
	req.Header.Set("X-Prism-Account-ID", "private-identity")
	req.Header.Set("Authorization", "secret-client-key")
	payload, _ := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol", "input": "private-prompt", "reasoning": map[string]any{"effort": "xhigh"},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "description": "private-description" + strings.Repeat("x", 32000)}},
	})
	diagnostic := NewPrismRequestLog(ctx, &config.Config{}, req, payload, "fixture-account")
	diagnostic.ResponseHeaders(422, http.Header{"Set-Cookie": []string{"secret-cookie"}})
	diagnostic.Response(422, []byte(`{"error":{"type":"invalid_tools","message":"Tool description is invalid"},"debug":"private-response"}`), payload)
	entries := hook.AllEntries()
	if len(entries) != 2 || entries[0].Data["event"] != "prism_request_start" || entries[1].Data["event"] != "prism_response_received" {
		t.Fatalf("events=%v", entries)
	}
	fields := entries[1].Data
	if fields["request_id"] != "request-fixture" || fields["description_issue"] != "too_long" || fields["tool_path"] != "tools.0.description" {
		t.Fatalf("missing diagnostics: %v", fields)
	}
	encoded, _ := json.Marshal(entries)
	captured := string(encoded)
	value, exists := ginCtx.Get(logging.DeferredAPIRequestContextKey)
	if !exists {
		t.Fatal("request log did not capture the deferred Prism attempt")
	}
	for _, request := range value.([]logging.DeferredAPIRequest) {
		captured += string(request())
	}
	for _, secret := range []string{"secret-oauth", "private-identity", "secret-client-key", "private-prompt", "private-description", "secret-cookie", "private-response"} {
		if strings.Contains(captured, secret) {
			t.Errorf("diagnostics exposed %s", secret)
		}
	}
	if req.Header.Get("X-Prism-OAuth-Token") != "secret-oauth" {
		t.Fatal("logging altered the outgoing credential")
	}
}

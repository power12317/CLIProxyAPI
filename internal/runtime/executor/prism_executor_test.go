package executor

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func prismTestAuth() *coreauth.Auth {
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"123"}}`)) + ".signature"
	return &coreauth.Auth{ID: "prism-auth", Provider: "codex", Metadata: map[string]any{
		"auth_kind": coreauth.AuthKindOAuth, "access_token": token, "account_id": "123", coreauth.AttributePrismBrowser: true,
	}}
}

func TestPrismExecutorP1TextAndUsageUnavailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer bridge-key" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-Prism-Account-ID"); got != "123" {
			t.Errorf("account id = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("stream was not forced: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"usage\":null}}\n\n")
	}))
	defer server.Close()
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	cfg := &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true, AdapterURL: server.URL + "/v1"}}}
	exec := NewPrismExecutor(cfg)
	result, err := exec.ExecuteStream(t.Context(), prismTestAuth(), coreexecutor.Request{Model: prismModel, Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if !strings.Contains(string(payload), "response.completed") || calls.Load() != 1 {
		t.Fatalf("payload=%s calls=%d", payload, calls.Load())
	}
}

func TestPrismExecutorNonStreamForcesUnknownUsageToNull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","model":"gpt-5.6-sol","status":"completed","output":[]}`)
	}))
	defer server.Close()
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	cfg := &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true, AdapterURL: server.URL + "/v1"}}}
	result, err := NewPrismExecutor(cfg).Execute(t.Context(), prismTestAuth(), coreexecutor.Request{Model: prismModel, Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Payload), `"usage":null`) {
		t.Fatalf("payload=%s", result.Payload)
	}
}

func TestPrismExecutorRejectsUnsupportedModelBeforeAdapter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	cfg := &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true, AdapterURL: server.URL + "/v1"}}}
	_, err := NewPrismExecutor(cfg).Execute(t.Context(), prismTestAuth(), coreexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err == nil || !strings.Contains(err.Error(), "unsupported_model") {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("adapter was contacted for unsupported model")
	}
}

func TestCodexAutoExecutorRoutesPrismBeforeBasispoints(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","model":"gpt-5.6-sol","status":"completed","output":[],"usage":null}`)
	}))
	defer server.Close()
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, Prism: config.CodexPrismConfig{Enabled: true, AdapterURL: server.URL + "/v1"}}}
	_, err := NewCodexAutoExecutor(cfg).Execute(t.Context(), prismTestAuth(), coreexecutor.Request{Model: prismModel, Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("adapter calls = %d, want 1", calls.Load())
	}
}

func TestCodexAutoExecutorGlobalPrismAppliesToOAuthWithoutAccountField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","model":"gpt-5.6-sol","status":"completed","output":[],"usage":null}`)
	}))
	defer server.Close()
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	auth := prismTestAuth()
	delete(auth.Metadata, coreauth.AttributePrismBrowser)
	cfg := &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true, AdapterURL: server.URL + "/v1"}}}
	if _, err := NewCodexAutoExecutor(cfg).Execute(t.Context(), auth, coreexecutor.Request{Model: prismModel, Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); err != nil {
		t.Fatal(err)
	}
}

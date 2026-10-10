package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func prismTestAuth() *coreauth.Auth {
	return &coreauth.Auth{ID: "prism-auth", Provider: "codex", Metadata: map[string]any{
		"auth_kind": coreauth.AuthKindOAuth, "access_token": "synthetic-oauth", "account_id": "d66399a1-1ca1-4a58-9db5-d3a5627d4f93",
	}}
}
func prismResponse(model string) []byte {
	data, _ := json.Marshal(map[string]any{"id": "resp-test", "object": "response", "status": "completed", "model": model, "usage": nil, "output": []any{map[string]any{"id": "msg-test", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "answer", "annotations": []any{}}}}}})
	return data
}
func prismConfig(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_ADAPTER_PORT", parsed.Port())
	return &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true}}}
}
func prismOptions() core.Options {
	return core.Options{SourceFormat: translator.FormatOpenAIResponse, Headers: http.Header{"Session-Id": []string{"same-session"}}, Metadata: map[string]any{core.CallerScopeMetadataKey: "verified-caller"}}
}

func TestPrismModelsEffortsAndBufferedEvents(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if _, present := r.Header["Authorization"]; present {
			t.Error("loopback request must not carry bridge authorization")
		}
		if len(r.Header.Get("X-Prism-Account-ID")) != 64 || len(r.Header.Get("X-Prism-Caller-ID")) != 64 || len(r.Header.Get("X-Prism-Session-ID")) != 64 {
			t.Error("private identities not forwarded")
		}
		if r.Header.Get("X-Prism-OAuth-Token") != "synthetic-oauth" {
			t.Error("OAuth not forwarded")
		}
		response := prismResponse(gjson.GetBytes(body, "model").String())
		w.Header().Set("X-Effort", gjson.GetBytes(body, "reasoning.effort").String())
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			response = helps.PrismEvents(response)
		}
		_, _ = w.Write(response)
	}))
	defer server.Close()
	for _, model := range registry.PrismModelIDs() {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				payload, _ := json.Marshal(map[string]any{"model": model, "input": "hello", "reasoning": map[string]any{"effort": effort}})
				exec := NewPrismExecutor(prismConfig(t, server.URL))
				req := core.Request{Model: model, Payload: payload}
				opts := prismOptions()
				opts.Headers.Set("Authorization", "Bearer synthetic-client-key")
				response, err := exec.Execute(t.Context(), prismTestAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if response.Headers.Get("X-Effort") != effort || response.Headers.Get("X-Prism-Usage") != "unavailable" {
					t.Fatalf("headers %v", response.Headers)
				}
				stream, err := exec.ExecuteStream(t.Context(), prismTestAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if !strings.Contains(string(chunk.Payload), "response.output_text.done") || strings.Contains(string(chunk.Payload), ".delta") {
						t.Fatal("incorrect terminal events")
					}
				}
			})
		}
	}
	if calls.Load() != 32 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestPrismModelAliasAndThinkingSuffix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "gpt-6.1-sol" || gjson.GetBytes(body, "reasoning.effort").String() != "high" {
			t.Errorf("wrong mapped request: %s", body)
		}
		_, _ = w.Write(prismResponse("gpt-6.1-sol"))
	}))
	defer server.Close()
	_, err := NewPrismExecutor(prismConfig(t, server.URL)).Execute(t.Context(), prismTestAuth(), core.Request{Model: "gpt-6.1-sol(high)", Payload: []byte(`{"model":"my-sol","reasoning":{"effort":"low"},"input":"hi"}`)}, prismOptions())
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrismSwitchAndNoNativeFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"prism_unavailable","message":"fixture adapter unavailable"}}`))
	}))
	defer server.Close()
	cfg := prismConfig(t, server.URL)
	exec := NewPrismExecutor(cfg)
	auth := prismTestAuth()
	for _, model := range registry.PrismModelIDs() {
		if !exec.routes(auth, model) {
			t.Fatalf("excluded %s", model)
		}
	}
	if exec.routes(auth, "gpt-6-astra") {
		t.Fatal("native model was claimed")
	}
	cfg.Codex.Prism.Enabled = false
	for _, model := range registry.PrismModelIDs() {
		if exec.routes(auth, model) {
			t.Fatalf("disabled switch routed %s through Prism", model)
		}
	}
	cfg.Codex.Prism.Enabled = true
	cfg.Codex.Basispoints.Enabled = true
	auto := NewCodexAutoExecutor(cfg)
	req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hi"}`)}
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = auto.ExecuteStream(t.Context(), auth, req, prismOptions())
		} else {
			_, err = auto.Execute(t.Context(), auth, req, prismOptions())
		}
		if err == nil || !strings.Contains(err.Error(), "fixture adapter unavailable") {
			t.Fatalf("stream=%t native fallback: %v", stream, err)
		}
		stop, ok := err.(interface{ IsRequestStop() bool })
		if !ok || !stop.IsRequestStop() {
			t.Fatal("request may be replayed")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("adapter calls = %d, want one per request", calls.Load())
	}
}

func TestPrismRejectsWebsocketBeforeDispatch(t *testing.T) {
	exec := NewPrismExecutor(prismConfig(t, "http://127.0.0.1:8319"))
	for _, ctx := range []context.Context{core.WithDownstreamWebsocket(t.Context()), core.WithRequiredUpstreamWebsocket(t.Context())} {
		_, err := exec.Execute(ctx, prismTestAuth(), core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hi"}`)}, prismOptions())
		if err == nil || !strings.Contains(err.Error(), "unsupported_endpoint") {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestPrismClientToolEvents(t *testing.T) {
	body := []byte(`{"id":"r","model":"gpt-6.1-sol","status":"completed","usage":null,"output":[{"type":"function_call","status":"completed","id":"i","call_id":"c","name":"lookup","namespace":"fs","arguments":"{\"path\":\"a\"}"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(helps.PrismEvents(body)) }))
	defer server.Close()
	cfg := prismConfig(t, server.URL)
	payload := []byte(`{"model":"gpt-6.1-sol","input":"hi","tools":[{"type":"namespace","name":"fs","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`)
	req := core.Request{Model: "gpt-6.1-sol", Payload: payload}
	result, err := NewPrismExecutor(cfg).ExecuteStream(t.Context(), prismTestAuth(), req, prismOptions())
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if !strings.Contains(string(chunk.Payload), "response.function_call_arguments.done") {
			t.Fatal("tool events lost")
		}
	}
}

func TestPrismPayloadRulesAreAppliedOnceAfterBuiltins(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, filterEffort := range []bool{false, true} {
			name := "json"
			if stream {
				name = "sse"
			}
			if filterEffort {
				name += "/filter"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if gjson.GetBytes(body, "input.#").Int() != 2 || gjson.GetBytes(body, "input.0.content").String() != "second" {
						t.Errorf("filter was skipped or repeated: %s", body)
					}
					if gjson.GetBytes(body, "model").String() != "gpt-5.6-sol" || gjson.GetBytes(body, "reasoning.summary").String() != "detailed" || gjson.GetBytes(body, "instructions").String() != "configured" {
						t.Errorf("override/default lost: %s", body)
					}
					effort := gjson.GetBytes(body, "reasoning.effort")
					if (filterEffort && effort.Exists()) || (!filterEffort && effort.String() != "high") {
						t.Errorf("built-in reasoning won over user rules: %s", body)
					}
					if gjson.GetBytes(body, "stream").Exists() {
						t.Errorf("stream was restored after filtering: %s", body)
					}
					_, _ = w.Write(prismResponse("gpt-5.6-sol"))
				}))
				defer server.Close()
				cfg := prismConfig(t, server.URL)
				targets := []config.PayloadModelRule{{Name: "gpt-6.1-sol", Protocol: "codex", FromProtocol: "openai-response", Headers: map[string]string{"X-Rule": "enabled"}}}
				filters := []string{"input.0", "stream"}
				if filterEffort {
					filters = append(filters, "reasoning.effort")
				}
				cfg.Payload = config.PayloadConfig{
					Default: []config.PayloadRule{{Models: targets, Params: map[string]any{"instructions": "configured"}}},
					Override: []config.PayloadRule{{Models: targets, Params: map[string]any{
						"model": "gpt-5.6-sol", "reasoning.effort": "high", "reasoning.summary": "detailed",
					}}},
					Filter: []config.PayloadFilterRule{{Models: targets, Params: filters}},
				}
				req := core.Request{Model: "gpt-6.1-sol(xhigh)", Payload: []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"first"},{"role":"assistant","content":"second"},{"role":"user","content":"third"}]}`)}
				opts := prismOptions()
				opts.Headers.Set("X-Rule", "enabled")
				exec := NewPrismExecutor(cfg)
				if stream {
					result, err := exec.ExecuteStream(t.Context(), prismTestAuth(), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil || !strings.Contains(string(chunk.Payload), "response.completed") {
							t.Fatalf("invalid downstream SSE: %s %v", chunk.Payload, chunk.Err)
						}
					}
				} else if _, err := exec.Execute(t.Context(), prismTestAuth(), req, opts); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 {
					t.Fatalf("adapter calls = %d", calls.Load())
				}
			})
		}
	}
}

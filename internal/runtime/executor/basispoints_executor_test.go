package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBasispointsRoutesEveryModelAndPreservesEffort(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: true, ResponseSteering: true}}
	executor := NewCodexAutoExecutor(cfg)
	auth := &coreauth.Auth{ID: "basispoints-route-test", Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account", "websockets": true}}
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-luna", "future-model"} {
		for _, effort := range []string{"", "low", "max", "ultra"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				body := `{"model":"` + model + `","input":"hello"}`
				if effort != "" {
					body = `{"model":"` + model + `","input":"hello","reasoning":{"effort":"` + effort + `"}}`
				}
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != basispoints.ResponsesURL {
						t.Fatalf("wrong upstream: %s", r.URL)
					}
					for k, v := range map[string]string{"Authorization": "Bearer test-token", "Chatgpt-Account-Id": "test-account", "X-OpenAI-Account-Id": "test-account", "X-Basispoints-Auth-Mode": "chatgpt"} {
						if r.Header.Get(k) != v {
							t.Fatalf("header %s = %q", k, r.Header.Get(k))
						}
					}
					raw, _ := io.ReadAll(r.Body)
					wireEffort := effort
					if model == "gpt-6-astra" && effort == "max" {
						wireEffort = "xhigh"
						if gjson.GetBytes(raw, "input.0.type").String() != "configuration_update" || gjson.GetBytes(raw, "input.0.reasoning.effort").String() != "max" {
							t.Fatal("requested max configuration missing")
						}
					}
					if gjson.GetBytes(raw, "model").String() != model || gjson.GetBytes(raw, "reasoning_effort").String() != wireEffort {
						t.Fatalf("request changed: %s", raw)
					}
					if effort == "" && gjson.GetBytes(raw, "reasoning_effort").Exists() {
						t.Fatal("default effort inserted")
					}
					if r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("Content-Encoding") != "" {
						t.Fatal("Codex-specific transport leaked")
					}
					status, payload := 200, fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"resp_test","model":%q,"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n", model)
					if wireEffort == "max" || wireEffort == "ultra" {
						status, payload = 422, `{"detail":"unsupported effort"}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
				})))
				result, err := executor.Execute(ctx, auth, coreexecutor.Request{Model: model, Payload: []byte(body)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
				if calls != 1 {
					t.Fatalf("calls = %d", calls)
				}
				if (effort == "max" && model != "gpt-6-astra") || effort == "ultra" {
					if err == nil || err.(interface{ StatusCode() int }).StatusCode() != 422 || err.Error() != `{"detail":"unsupported effort"}` {
						t.Fatalf("upstream error changed: %v", err)
					}
				} else if err != nil || gjson.GetBytes(result.Payload, "model").String() != model {
					t.Fatalf("response = %s, %v", result.Payload, err)
				}
			})
		}
	}
}

func TestBasispointsManagerToolReplayAllowsAccountRotation(t *testing.T) {
	const model = "basispoints-account-replay"
	cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
	for _, id := range []string{"bps-replay-account-a", "bps-replay-account-b"} {
		auth := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "token", "account_id": id}}
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	calls := 0
	firstAccount := ""
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		payload := `{"status":"completed","output":[]}`
		if calls == 1 {
			firstAccount = r.Header.Get("Chatgpt-Account-Id")
			payload = `{"status":"completed","output":[{"type":"function_call","id":"native_account_replay","call_id":"call_account_replay","name":"run_officejs","arguments":"{\"summary\":\"test\",\"references\":[],\"code\":\"{\\\"tool\\\":\\\"weather\\\",\\\"args\\\":{}}\"}"}]}`
		} else {
			if r.Header.Get("Chatgpt-Account-Id") == firstAccount {
				t.Error("Basispoints must preserve normal account rotation")
			}
			if gjson.GetBytes(raw, "input.2.id").String() == "native_account_replay" || gjson.GetBytes(raw, "input.2.name").String() != "run_officejs" {
				t.Errorf("native item missing: %s", raw)
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})))
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: t.Name()}}
	body := []byte(`{"model":"` + model + `","input":[{"role":"user","content":"weather"}],"tools":[{"type":"function","name":"weather"}]}`)
	first, err := manager.Execute(ctx, []string{"codex"}, coreexecutor.Request{Model: model, Payload: body}, opts)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(gjson.GetBytes(first.Payload, "output.0").Raw))
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"function_call_output","call_id":"call_account_replay","output":"18C"}`))
	_, err = manager.Execute(ctx, []string{"codex"}, coreexecutor.Request{Model: model, Payload: body}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d", calls)
	}
}

func TestBasispointsDisabledUsesOriginalUpstream(t *testing.T) {
	executor := NewCodexAutoExecutor(&config.Config{})
	auth := &coreauth.Auth{ID: "disabled", Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" {
			t.Fatalf("upstream = %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_original\",\"output\":[]}}\n\n"))}, nil
	})))
	_, err := executor.Execute(ctx, auth, coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBasispointsStreamingAndCancellation(t *testing.T) {
	executor := NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}})
	auth := &coreauth.Auth{ID: "stream-cancel", Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		go func() { <-r.Context().Done(); _ = reader.CloseWithError(r.Context().Err()) }()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	})))
	result, err := executor.ExecuteStream(ctx, auth, coreexecutor.Request{Model: "future", Payload: []byte(`{"model":"future","input":"hello"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	}()
	chunk := <-result.Chunks
	if !strings.Contains(string(chunk.Payload), "hello") {
		t.Fatalf("not incremental: %s %v", chunk.Payload, chunk.Err)
	}
	cancel()
	for range result.Chunks {
	}
}

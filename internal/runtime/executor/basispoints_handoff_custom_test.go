package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsHandoffAndCustomExecThroughExecutor(t *testing.T) {
	const handoff = "<codex_delegation>\nKeep the complete handoff and all original tool capabilities.\n</codex_delegation>"
	for _, mode := range []struct{ unmarked, stream bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		unmarked, stream := mode.unmarked, mode.stream
		t.Run(fmt.Sprintf("unmarked=%v/stream=%v", unmarked, stream), func(t *testing.T) {
			cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
			executor := NewCodexAutoExecutor(cfg)
			auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"}}
			body, _ := json.Marshal(map[string]any{
				"model": "gpt-5.6-luna", "reasoning": map[string]any{"effort": "medium"}, "stream": stream,
				"input": []any{
					map[string]any{"role": "user", "content": "Continue this task"},
					map[string]any{"type": "function_call_output", "id": "fco_fixture", "namespace": "codex_app", "name": "create_thread", "output": handoff},
					map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Run JavaScript to call client tools", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: CODE"}}}}}},
				},
			})
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != basispoints.ResponsesURL {
					t.Fatalf("wrong upstream: %s", r.URL)
				}
				wire, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(wire, "model").String() != "gpt-5.6-luna" || gjson.GetBytes(wire, "reasoning_effort").String() != "medium" {
					t.Fatal("model or effort changed")
				}
				if gjson.GetBytes(wire, "input.2.type").String() != "message" || gjson.GetBytes(wire, "input.2.content.0.text").String() != "Tool output from codex_app__create_thread:\n"+handoff {
					t.Fatal("upstream handoff is still an orphan result")
				}
				if !strings.Contains(gjson.GetBytes(wire, "input.0.content.0.text").String(), "tools.exec_command") {
					t.Fatal("custom invocation guide missing from upstream request")
				}
				code, _ := json.Marshal(map[string]any{"name": "functions.exec", "arguments": map[string]any{"cmd": "pwd", "workdir": "/tmp/project", "yield_time_ms": 10000, "max_output_tokens": 12000}})
				if unmarked {
					code = []byte("const result = await tools.exec_command({cmd: 'pwd'}); text(result);")
				}
				args, _ := json.Marshal(map[string]any{"summary": "Inspect project and apps", "code": string(code)})
				response := map[string]any{"status": "completed", "model": "gpt-5.6-luna", "output": []any{map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_fixture", "call_id": "call_fixture", "arguments": string(args)}}}
				var payload strings.Builder
				for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"status": "in_progress"}}, {"type": "response.in_progress", "response": map[string]any{"status": "in_progress"}}, {"type": "response.completed", "response": response}} {
					raw, _ := json.Marshal(event)
					payload.WriteString("data: " + string(raw) + "\n\n")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload.String()))}, nil
			})))
			req := coreexecutor.Request{Model: "gpt-5.6-luna", Payload: body}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			var completed []byte
			if stream {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					if value := basispoints.CompletedResponse(chunk.Payload); len(value) > 0 {
						completed = value
					}
				}
			} else {
				result, err := executor.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				completed = result.Payload
			}
			if calls != 1 {
				t.Fatalf("unexpected retry: %d", calls)
			}
			if gjson.GetBytes(completed, "output.0.type").String() != "custom_tool_call" || gjson.GetBytes(completed, "output.0.name").String() != "exec" || !strings.Contains(gjson.GetBytes(completed, "output.0.input").String(), "await tools.exec_command(") {
				t.Fatalf("client tool conversion failed: %s", completed)
			}
		})
	}
}

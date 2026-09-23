package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteResponsesLiteHeaderInjectsFunctionImageTool(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Headers:      headers,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if tools := gjson.GetBytes(gotBody, "tools"); tools.Exists() {
		t.Fatalf("unexpected tools in responses-lite upstream payload: %s", tools.Raw)
	}
	if !helps.HasCodexImageTool(gotBody) || util.ClassifyCodexResponsesLiteTools(gotBody) != util.CodexResponsesLiteToolsCompatible {
		t.Fatalf("missing Lite image function: %s", gotBody)
	}
	parallelToolCalls := gjson.GetBytes(gotBody, "parallel_tool_calls")
	if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", gotBody)
	}
}

func TestCodexOfficialRequestPreservesLiteBodyAcrossTranslation(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	payload := []byte(`{"model":"gpt-5.6-sol","reasoning":{"context":"all_turns"},"parallel_tool_calls":false,"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\"}"},"tools":[{"type":"function","name":"exec","parameters":{}}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gotHeaders.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("Lite header = %q, want reconstructed Lite header; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("reasoning.context = %q, want all_turns", got)
	}
	if got := gjson.GetBytes(gotBody, "parallel_tool_calls"); got.Type != gjson.False {
		t.Fatalf("parallel_tool_calls = %s, want final true fixture", got.Raw)
	}
	if got := util.CollectResponsesToolWinners(gjson.ParseBytes(gotBody)); got["exec"].ToolType != "function" || got["image_gen__imagegen"].ToolType != "function" {
		t.Fatalf("expected original and image function declarations: %s", gotBody)
	}
}

func TestCodexOfficialRequestDoesNotReconstructLiteHeaderForNonLiteModel(t *testing.T) {
	headers := make(http.Header)
	body := []byte(`{"model":"gpt-5.5","tools":[{"type":"function","name":"exec"}]}`)
	ensureCodexResponsesLiteHeader(headers, body, "gpt-5.5", true)
	if got := headers.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("Lite header = %q, want no reconstructed header", got)
	}
}

func TestCodexResponsesLiteAutoConditionRequiresExactBodyFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "valid", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, want: true},
		{name: "missing context", body: `{"model":"gpt-5.6-luna","parallel_tool_calls":false}`, want: false},
		{name: "null context", body: `{"model":"gpt-5.6-luna","reasoning":{"context":null},"parallel_tool_calls":false}`, want: false},
		{name: "wrong context", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"false"},"parallel_tool_calls":false}`, want: false},
		{name: "missing parallel", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"}}`, want: false},
		{name: "null parallel", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"},"parallel_tool_calls":null}`, want: false},
		{name: "string parallel", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"},"parallel_tool_calls":"false"}`, want: false},
		{name: "number parallel", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"},"parallel_tool_calls":0}`, want: false},
		{name: "true parallel", body: `{"model":"gpt-5.6-luna","reasoning":{"context":"all_turns"},"parallel_tool_calls":true}`, want: false},
		{name: "model capability false", body: `{"model":"gpt-5.5","reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			model := gjson.GetBytes(body, "model").String()
			headers := make(http.Header)
			ensureCodexResponsesLiteHeader(headers, body, model, true)
			gotHeader := headers.Get(codexResponsesLiteHeader) == "true"
			if gotHeader != test.want {
				t.Fatalf("header enabled = %t, want %t", gotHeader, test.want)
			}
			mirrored := ensureCodexResponsesLiteMirror(body, model, true)
			gotMirror := gjson.GetBytes(mirrored, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String() == "true"
			if gotMirror != test.want {
				t.Fatalf("mirror enabled = %t, want %t; body=%s", gotMirror, test.want, mirrored)
			}
		})
	}
}

func TestCodexOfficialRequestInjectsImageToolWhenEnabled(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\"}"},"input":[]}`)
	got := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, &config.Config{}, nil, "/v1/responses", true)
	if gjson.GetBytes(got, "tools.0.type").String() != "image_generation" {
		t.Fatalf("official Codex request missing image tool: %s", got)
	}
}

func TestDisableImageGenerationRemovesLiteAdditionalTool(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"image_generation"},{"type":"function","name":"exec"}]}],"tool_choice":{"type":"image_generation"}}`)
	got := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}, nil, "/v1/responses", true)
	if gjson.GetBytes(got, "input.0.tools.0.type").String() != "function" {
		t.Fatalf("remaining tool = %s, want function; body=%s", gjson.GetBytes(got, "input.0.tools.0.type").Raw, got)
	}
	if gjson.GetBytes(got, "tool_choice").Exists() {
		t.Fatalf("image tool choice was not removed: %s", got)
	}
}

func TestCodexLiteHeaderIsPreservedWhenToolShapeIsNotLite(t *testing.T) {
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\"}"},"tools":[{"type":"image_generation"}]}`)
	ensureCodexResponsesLiteHeader(headers, body, "gpt-5.6-luna", true)
	if got := headers.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("Lite header = %q, want existing true header preserved", got)
	}
}

func TestCodexLiteMirrorIsPreservedWhenAlreadyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`)
	got := ensureCodexResponsesLiteMirror(body, "gpt-5.6-luna", true)
	if value := gjson.GetBytes(got, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String(); value != "false" {
		t.Fatalf("existing mirror = %q, want false", value)
	}
}

func TestPassthroughImageGenerationPolicyKeepsPayload(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}}
	got := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, cfg, nil, "/v1/responses", true)
	if string(got) != string(body) {
		t.Fatalf("passthrough changed body: got %s want %s", got, body)
	}
}

func TestCodexExecutorExecuteStreamResponsesLiteHeaderForcesParallelToolCallsFalse(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")

	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6-luna",
		Payload: []byte(`{"model":"gpt-5.6-luna","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Headers:      headers,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	parallelToolCalls := gjson.GetBytes(gotBody, "parallel_tool_calls")
	if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", gotBody)
	}
}

func TestCodexImagePolicy_ResponsesLiteMetadataInjectsFunction(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"input":[{"role":"user","content":"hello"}]}`)
	result := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, &config.Config{}, nil, "/v1/responses", false)

	if !helps.HasCodexImageTool(result) || gjson.GetBytes(result, "tools").Exists() || util.ClassifyCodexResponsesLiteTools(result) != util.CodexResponsesLiteToolsCompatible {
		t.Fatalf("expected Lite image function in additional_tools: %s", result)
	}
}

func TestCodexImagePolicy_ResponsesLiteBooleanMetadataInjectsFunction(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true},"input":[{"role":"user","content":"hello"}]}`)
	result := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, &config.Config{}, nil, "/v1/responses", false)

	if !helps.HasCodexImageTool(result) || util.ClassifyCodexResponsesLiteTools(result) != util.CodexResponsesLiteToolsCompatible {
		t.Fatalf("expected Lite image function: %s", result)
	}
}

func TestCodexImagePolicy_ResponsesLiteHeaderInjectsFunction(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`)
	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	result := mustApplyCodexImagePolicy(t, body, "gpt-5.6-sol", nil, &config.Config{}, headers, "/v1/responses", false)

	if !helps.HasCodexImageTool(result) || util.ClassifyCodexResponsesLiteTools(result) != util.CodexResponsesLiteToolsCompatible {
		t.Fatalf("expected Lite image function: %s", result)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteFalseMetadataStillInjectsTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"},"input":"hello"}`)
	result := ensureImageGenerationTool(body, "gpt-5.6-sol", nil)

	if got := gjson.GetBytes(result, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, result)
	}
}

func TestEnsureImageGenerationTool_NoTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools")
	if !tools.IsArray() {
		t.Fatalf("expected tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
	if arr[0].Get("output_format").String() != "png" {
		t.Fatalf("expected output_format=png, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ExistingToolsWithoutImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"get_weather","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "function" {
		t.Fatalf("expected first tool type=function, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_AlreadyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"webp"},{"type":"function","name":"f1"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools (no duplicate), got %d", len(arr))
	}
	if arr[0].Get("output_format").String() != "webp" {
		t.Fatalf("expected original output_format=webp preserved, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ImageGenNamespaceDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_FlattenedImageGenFunctionDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"image_gen.imagegen","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_SimilarNamespaceStillInjectsTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"namespace","name":"image_tools","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", tools[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_WebSearchAndImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"web_search"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "web_search" {
		t.Fatalf("expected first tool type=web_search, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_GPT53CodexSparkDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.3-codex-spark","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.3-codex-spark", nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for gpt-5.3-codex-spark, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_FreeCodexAuthDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	freeAuth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"plan_type": "free"},
	}
	result := ensureImageGenerationTool(body, "gpt-5.4", freeAuth)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for free codex auth, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

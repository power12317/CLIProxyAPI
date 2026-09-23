package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func assertCodexReservedWireFixtures(t *testing.T, body []byte) {
	t.Helper()
	for _, namespace := range []string{"image_gen", "web"} {
		raw, err := os.ReadFile("helps/codex_lite_fixtures/0.156.0/" + namespace + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var expected any
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("type").String() != "additional_tools" {
				continue
			}
			for _, tool := range item.Get("tools").Array() {
				if tool.Get("type").String() == "namespace" && tool.Get("name").String() == namespace {
					found = true
					var actual any
					if err := json.Unmarshal([]byte(tool.Raw), &actual); err != nil || !reflect.DeepEqual(actual, expected) {
						t.Fatalf("final upstream %s declaration differs from official fixture", namespace)
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing final upstream %s declaration", namespace)
		}
	}
}

func TestCodexReservedSchemaConflictStopsBeforeUpstream(t *testing.T) {
	for _, transport := range []string{"http", "http-stream", "ws", "ws-stream"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			cfg := &config.Config{}
			body := []byte(`{"model":"gpt-6-astra","input":[],"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {"true"}}}
			exec := NewCodexExecutor(cfg)
			ws := NewCodexWebsocketsExecutor(cfg)
			var err error
			switch transport {
			case "http":
				_, err = exec.Execute(context.Background(), codexTestAuth(server.URL), req, opts)
			case "http-stream":
				_, err = exec.ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
			case "ws":
				_, err = ws.Execute(context.Background(), codexTestAuth(server.URL), req, opts)
			case "ws-stream":
				_, err = ws.ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
			}
			if err == nil || gjson.Get(err.Error(), "error.code").String() != "codex_reserved_tool_schema_conflict" {
				t.Fatalf("missing reserved-schema error: %v", err)
			}
			if calls.Load() != 0 || !err.(cliproxyexecutor.RequestScopedError).IsRequestScoped() {
				t.Fatal("invalid schema was sent upstream or eligible for credential retry")
			}
		})
	}
}

func TestCodexReservedSchemaRespectsImagePolicyAndModel(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[],"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
	headers := http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {"true"}}
	for _, mode := range []config.DisableImageGenerationMode{config.DisableImageGenerationAll, config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough} {
		got := mustApplyCodexImagePolicy(t, body, "gpt-6-astra", nil, &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: mode}}, headers, "/v1/responses", true)
		if mode == config.DisableImageGenerationPassthrough && string(got) != string(body) || mode != config.DisableImageGenerationPassthrough && helps.HasCodexImageTool(got) {
			t.Fatalf("image policy changed: %s", got)
		}
	}
	got := mustApplyCodexImagePolicy(t, body, "gpt-6-astra", nil, &config.Config{}, nil, "/v1/responses", false)
	if string(got) != string(body) {
		t.Fatal("non-Lite user declaration changed")
	}
	request := []byte(`{"model":"gpt-6-astra","input":[],"tools":[{"type":"web_search"}],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`)
	request, _ = sjson.SetBytes(request, "client_metadata.x-codex-turn-metadata", "{}")
	got = mustApplyCodexImagePolicy(t, request, "gpt-6-astra", nil, &config.Config{}, headers, "/v1/responses", true)
	assertCodexReservedWireFixtures(t, got)
	if util.ClassifyCodexResponsesLiteTools(got) != util.CodexResponsesLiteToolsCompatible {
		t.Fatal("reserved functions no longer satisfy Lite tool types")
	}
}

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
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBasispointsSerializedRequestContract(t *testing.T) {
	const completed = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n"
	const rejected = `{"error":{"message":"422: Invalid request body.","type":"server_error","param":null,"code":null}}`
	for _, format := range []sdktranslator.Format{sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ tier, wantTier string }{
				{"", ""}, {`null`, ""}, {`"default"`, ""}, {`"auto"`, ""},
				{`"standard"`, ""}, {`"priority"`, "priority"}, {`"fast"`, "priority"}, {`"ultrafast"`, "ultrafast"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/tier=%s", format, stream, tc.tier), func(t *testing.T) {
					cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
					executor := NewCodexAutoExecutor(cfg)
					auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test-token", "account_id": "test-account"}}
					calls := 0
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						// A strict mock checks the serialized contract, not just selected
						// fields in the in-memory object, and reproduces the upstream 422.
						var wire struct {
							Model             string            `json:"model"`
							ModelSelection    string            `json:"model_selection"`
							Stream            bool              `json:"stream"`
							Store             bool              `json:"store"`
							Input             []json.RawMessage `json:"input"`
							ReasoningEffort   string            `json:"reasoning_effort"`
							PromptCacheKey    string            `json:"prompt_cache_key"`
							Metadata          map[string]string `json:"metadata"`
							ContextManagement json.RawMessage   `json:"context_management"`
							ServiceTier       *string           `json:"service_tier"`
						}
						decoder := json.NewDecoder(r.Body)
						decoder.DisallowUnknownFields()
						valid := decoder.Decode(&wire) == nil
						valid = valid && wire.Model == "gpt-6-astra" && wire.ModelSelection == "explicit" && wire.Stream && !wire.Store && wire.ReasoningEffort == "low"
						valid = valid && len(wire.ContextManagement) == 0 && wire.Metadata["attempt"] == "2"
						if tc.wantTier == "" {
							valid = valid && wire.ServiceTier == nil
						} else if wire.ServiceTier == nil || *wire.ServiceTier != tc.wantTier {
							t.Fatalf("explicit accelerated tier changed: %#v", wire.ServiceTier)
						}
						if !valid {
							t.Error("serialized request does not match the Basispoints schema")
						}
						status, body := http.StatusOK, completed
						if !valid || tc.wantTier != "" {
							status, body = http.StatusUnprocessableEntity, rejected
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})))
					body := []byte(`{"model":"gpt-6-astra","instructions":"Reply OK","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}],"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"max_output_tokens":1234,"context_management":null,"metadata":{"attempt":2},"prompt_cache_retention":"24h","client_metadata":{"x-codex-turn-metadata":"{}"}}`)
					if tc.tier != "" {
						body, _ = sjson.SetRawBytes(body, "service_tier", []byte(tc.tier))
					}
					req := coreexecutor.Request{Model: "gpt-6-astra", Payload: body}
					opts := coreexecutor.Options{SourceFormat: format}
					var err error
					if stream {
						var result *coreexecutor.StreamResult
						result, err = executor.ExecuteStream(ctx, auth, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								if !strings.Contains(string(chunk.Payload), "response.completed") {
									t.Fatalf("unexpected SSE response: %s", chunk.Payload)
								}
							}
						}
					} else {
						var result coreexecutor.Response
						result, err = executor.Execute(ctx, auth, req, opts)
						if err == nil && gjson.GetBytes(result.Payload, "status").String() != "completed" {
							t.Fatalf("unexpected JSON response: %s", result.Payload)
						}
					}
					if tc.wantTier == "" && err != nil {
						t.Fatal(err)
					}
					if tc.wantTier != "" && (err == nil || err.Error() != rejected || err.(interface{ StatusCode() int }).StatusCode() != 422) {
						t.Fatalf("upstream rejection was hidden or changed: %v", err)
					}
					if calls != 1 {
						t.Fatalf("request unexpectedly retried or fell back: %d calls", calls)
					}
				})
			}
		}
	}
}

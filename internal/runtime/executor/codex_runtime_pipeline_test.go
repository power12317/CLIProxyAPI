package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Compare the actual HTTP boundary with the actual bridge boundary, not a second
// copy of the request-building implementation.
func TestCodexRuntimeUsesFullCPARequestPipeline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, withPayload := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/payload=%v", stream, withPayload), func(t *testing.T) {
				captured := make(chan bridge.Request, 1)
				settings, auth := runtimeTestMaster(t, "pipeline-account", func(c *websocket.Conn, m runtimeTestRPC, request bridge.Request) {
					captured <- request
					runtimeTestAccept(c, m, request)
					runtimeTestFinish(c, request, runtimeTerminal)
				})
				auth.Metadata["access_token"] = "test-token"
				auth.Metadata["account_id"] = "test-account"
				cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}, CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: "fast"}}
				if withPayload {
					models := []config.PayloadModelRule{{Name: "runtime-model", Protocol: "codex"}}
					cfg.Payload = config.PayloadConfig{
						Default:    []config.PayloadRule{{Models: models, Params: map[string]any{"configured_default": "default-value"}}},
						DefaultRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"configured_raw": `{"enabled":true}`}}},
						Override: []config.PayloadRule{{Models: models, Params: map[string]any{
							"model": "configured-model", "service_tier": "flex", "prompt_cache_key": "configured-cache", "instructions": "configured-instructions",
						}}},
						OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"text": `{"verbosity":"low"}`}}},
						Filter:      []config.PayloadFilterRule{{Models: models, Params: []string{"tools", "parallel_tool_calls", "include", "client_metadata.x-codex-installation-id"}}},
					}
				}
				body := []byte(`{"model":"runtime-model","input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n<current_date>2026-10-07</current_date>\n<timezone>Asia/Shanghai</timezone>\n</environment_context>"}]}],"client_metadata":{"session_id":"source-session","thread_id":"source-thread","x-codex-turn-metadata":"{\"session_id\":\"source-session\",\"thread_id\":\"source-thread\",\"turn_id\":\"source-turn\",\"timezone\":\"Asia/Shanghai\"}"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"prompt_cache_key":"source-cache","service_tier":"default","previous_response_id":"remove-me","generate":false,"prompt_cache_retention":"24h"}`)
				req := coreexecutor.Request{Model: "runtime-model", Payload: body}
				opts := runtimeTestOptions()
				opts.Stream = stream
				var directBody []byte
				directAuth := auth.Clone()
				delete(directAuth.Metadata, "codex_cli")
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(request *http.Request) (*http.Response, error) {
					wire, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					if request.Header.Get("Content-Encoding") == "zstd" {
						decoder, errDecoder := zstd.NewReader(nil)
						if errDecoder != nil {
							return nil, errDecoder
						}
						defer decoder.Close()
						wire, err = decoder.DecodeAll(wire, nil)
						if err != nil {
							return nil, err
						}
					}
					directBody = wire
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + runtimeTerminal + "\n\n"))}, nil
				})))
				direct := NewCodexExecutor(cfg)
				runtime := NewCodexRuntimeExecutor(cfg)
				if stream {
					result, err := direct.ExecuteStream(ctx, directAuth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
					result, err = runtime.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err := direct.Execute(ctx, directAuth, req, opts); err != nil {
						t.Fatal(err)
					}
					if _, err := runtime.Execute(context.Background(), auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				request := <-captured
				if request.SessionID == "" {
					t.Fatal("missing routing scope")
				}
				equalRuntimeJSON(t, directBody, request.Request)
				if strings.Contains(string(request.Request), "Asia/Shanghai") || !strings.Contains(string(request.Request), "Asia/Singapore") {
					t.Fatalf("CPA timezone processing was skipped: %s", request.Request)
				}
				if gjson.GetBytes(request.Request, "input.#").Int() != 1 {
					t.Fatal("request preparation appended a message")
				}
				if withPayload {
					for path, value := range map[string]string{"model": "configured-model", "service_tier": "flex", "prompt_cache_key": "configured-cache", "instructions": "configured-instructions", "configured_default": "default-value", "text.verbosity": "low"} {
						if gjson.GetBytes(request.Request, path).String() != value {
							t.Errorf("final payload rule lost: %s", path)
						}
					}
					if !gjson.GetBytes(request.Request, "configured_raw.enabled").Bool() {
						t.Error("default-raw was not applied")
					}
					for _, path := range []string{"tools", "parallel_tool_calls", "include", "client_metadata.x-codex-installation-id"} {
						if gjson.GetBytes(request.Request, path).Exists() {
							t.Errorf("filtered field was restored: %s", path)
						}
					}
				} else if gjson.GetBytes(request.Request, "service_tier").String() != "priority" {
					t.Fatal("CPA Fast Mode did not reach the bridge")
				}
			})
		}
	}
}

func TestCodexRuntimeCPARequestRetainsCrossProtocolTranslation(t *testing.T) {
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
		t.Run(source.String(), func(t *testing.T) {
			settings, auth := runtimeTestMaster(t, "translation", func(c *websocket.Conn, m runtimeTestRPC, request bridge.Request) {
				if gjson.GetBytes(request.Request, "messages").Exists() || !gjson.GetBytes(request.Request, "input").IsArray() || gjson.GetBytes(request.Request, "instructions").String() != "configured" {
					t.Errorf("translation or final rules missing: %s", request.Request)
				}
				runtimeTestAccept(c, m, request)
				runtimeTestFinish(c, request, runtimeTerminal)
			})
			e := runtimeTestExecutor(settings)
			e.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "runtime-model"}}, Params: map[string]any{"instructions": "configured"}}}
			opts := runtimeTestOptions()
			opts.SourceFormat = source
			if _, err := e.Execute(context.Background(), auth, coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":50}`)}, opts); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexRuntimeResponseRestoresMultiAgentNamespace(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			settings, auth := runtimeTestMaster(t, "multi-agent", func(c *websocket.Conn, m runtimeTestRPC, request bridge.Request) {
				if !strings.Contains(string(request.Request), "collaboration-optimize") {
					t.Error("CPA multi-agent request optimization was skipped")
				}
				runtimeTestAccept(c, m, request)
				runtimeTestFinish(c, request, `{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","namespace":"collaboration-optimize","name":"spawn_agent","arguments":"{}","call_id":"call_1"}]}}`)
			})
			e := runtimeTestExecutor(settings)
			e.cfg.Client.Codex.OptimizeMultiAgentV2 = true
			req := coreexecutor.Request{Model: "gpt-5.4", Payload: codexSpawnAgentTestPayload()}
			opts := runtimeTestOptions()
			var output []byte
			if stream {
				result, err := e.ExecuteStream(codexSpawnAgentTestContext(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output = append(output, chunk.Payload...)
				}
			} else {
				result, err := e.Execute(codexSpawnAgentTestContext(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				output = result.Payload
			}
			if strings.Contains(string(output), "collaboration-optimize") || !strings.Contains(string(output), `"namespace":"collaboration"`) {
				t.Fatalf("CPA response restoration was skipped: %s", output)
			}
		})
	}
}

func TestCodexRuntimeResponseClearsInvalidReasoningReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, rpcFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/rpc=%v", stream, rpcFailure), func(t *testing.T) {
				internalcache.ClearCodexReasoningReplayCache()
				t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
				encrypted := validCodexReasoningEncryptedContentForTestSeed(9)
				internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "claude:pipeline-invalid:agent:main", []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+encrypted+`"}`))
				settings, auth := runtimeTestMaster(t, "replay", func(c *websocket.Conn, m runtimeTestRPC, request bridge.Request) {
					if rpcFailure {
						_ = c.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "Invalid signature in thinking block", "data": map[string]any{"httpStatus": 400, "body": `{"error":{"message":"Invalid signature in thinking block","type":"invalid_request_error"}}`}}})
						return
					}
					runtimeTestAccept(c, m, request)
					runtimeTestFinish(c, request, `{"type":"response.failed","response":{"status":"failed","error":{"message":"Invalid signature in thinking block","type":"invalid_request_error"}}}`)
				})
				opts := runtimeTestOptions()
				opts.SourceFormat = sdktranslator.FormatClaude
				req := coreexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"\",\"session_id\":\"pipeline-invalid\"}"},"messages":[{"role":"user","content":"next"}]}`)}
				e := runtimeTestExecutor(settings)
				var err error
				if stream {
					var result *coreexecutor.StreamResult
					result, err = e.ExecuteStream(context.Background(), auth, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(context.Background(), auth, req, opts)
				}
				if err == nil {
					t.Fatal("expected invalid signature error")
				}
				if _, ok := internalcache.GetCodexReasoningReplayItem("gpt-5.4", "claude:pipeline-invalid:agent:main"); ok {
					t.Fatal("CPA invalid-signature replay cleanup was skipped")
				}
			})
		}
	}
}

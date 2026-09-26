package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func fastModeExpectedTier(mode, input string) string {
	switch mode {
	case "default":
		return ""
	case "fast":
		return "priority"
	case "ultrafast":
		return "ultrafast"
	}
	switch input {
	case "fast", "priority":
		return "priority"
	case "ultrafast":
		return "ultrafast"
	default:
		return ""
	}
}

func TestCodexFastModeHTTPAndBasispoints(t *testing.T) {
	for _, basispoints := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"auto", "default", "fast", "ultrafast"} {
				for _, input := range []string{"", "default", "fast", "priority", "ultrafast"} {
					t.Run(fmt.Sprintf("basispoints=%v/stream=%v/%s/input=%s", basispoints, stream, mode, input), func(t *testing.T) {
						cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: mode}}
						cfg.Codex.Basispoints.Enabled = basispoints
						cfg.DisableImageGeneration = config.DisableImageGenerationAll
						var captured capturedCodexRequest
						server := newCodexRoutingHintServer(t, &captured)
						defer server.Close()
						auth := codexOAuthTestAuth(server.URL)
						auth.ID = t.Name()
						ctx := context.Background()
						if basispoints {
							ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
								if r.URL.Host != "bps.openai.com" {
									t.Errorf("unexpected upstream: %s", r.URL)
								}
								sent, errRead := io.ReadAll(r.Body)
								if errRead != nil {
									return nil, errRead
								}
								captured.serviceTier = gjson.GetBytes(sent, "service_tier")
								payload := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n"
								return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
							})))
						}
						request := map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}}}, "reasoning": map[string]string{"effort": "low"}}
						if input != "" {
							request["service_tier"] = input
						}
						body, errMarshal := json.Marshal(request)
						if errMarshal != nil {
							t.Fatal(errMarshal)
						}
						req := coreexecutor.Request{Model: "gpt-6-astra", Payload: body}
						opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body, Stream: stream}
						executor := NewCodexAutoExecutor(cfg)
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
						want := fastModeExpectedTier(mode, input)
						if captured.serviceTier.String() != want || (want == "" && captured.serviceTier.Exists()) {
							t.Fatalf("upstream tier=%s want=%q", captured.serviceTier.Raw, want)
						}
						if !basispoints {
							hint := "model=gpt-6-astra"
							if want != "" {
								hint += ";tier=" + want
							}
							if captured.routingHint != hint {
								t.Fatalf("routing hint=%q want=%q", captured.routingHint, hint)
							}
						}
						if gjson.GetBytes(req.Payload, "service_tier").String() != input {
							t.Fatal("client payload changed")
						}
					})
				}
			}
		}
	}
}

func TestCodexFastModeCompact(t *testing.T) {
	for _, mode := range []string{"auto", "default", "fast", "ultrafast"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: mode}}
			var captured capturedCodexRequest
			server := newCodexRoutingHintServer(t, &captured)
			defer server.Close()
			_, err := NewCodexExecutor(cfg).Execute(context.Background(), codexOAuthTestAuth(server.URL), coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte("{\"model\":\"gpt-6-astra\",\"input\":[],\"service_tier\":\"ultrafast\"}")}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"})
			if err != nil {
				t.Fatal(err)
			}
			want := fastModeExpectedTier(mode, "ultrafast")
			if captured.serviceTier.String() != want || (want == "" && captured.serviceTier.Exists()) {
				t.Fatalf("compact tier=%s want=%q", captured.serviceTier.Raw, want)
			}
		})
	}
}

func TestCodexFastModeWebSocket(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"auto", "default", "fast", "ultrafast"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				requests := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() {
						if errClose := conn.Close(); errClose != nil {
							t.Log(errClose)
						}
					}()
					_, body, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Error(errRead)
						return
					}
					requests <- body
					payload := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"ws-fast-mode\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}")
					if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
						t.Error(errWrite)
					}
				}))
				defer server.Close()
				cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: mode}}
				cfg.DisableImageGeneration = config.DisableImageGenerationAll
				executor := NewCodexWebsocketsExecutor(cfg)
				auth := codexOAuthTestAuth(server.URL)
				auth.ID = t.Name()
				req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte("{\"model\":\"gpt-6-astra\",\"input\":[],\"service_tier\":\"priority\"}")}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
				if stream {
					result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				select {
				case body := <-requests:
					tier := gjson.GetBytes(body, "service_tier")
					want := fastModeExpectedTier(mode, "priority")
					if tier.String() != want || (want == "" && tier.Exists()) {
						t.Fatalf("websocket tier=%s want=%q", tier.Raw, want)
					}
				default:
					t.Fatal("upstream websocket received no request")
				}
			})
		}
	}
}

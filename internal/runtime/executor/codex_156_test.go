package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodex156LiteInjectionAcrossTransports(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		for _, intent := range []string{"header", "body-mirror", "official-without-header", "configured"} {
			for _, transport := range []string{"http", "http-stream", "ws", "ws-stream"} {
				t.Run(model+"/"+intent+"/"+transport, func(t *testing.T) {
					captured := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get(codexResponsesLiteHeader) != "true" {
							t.Error("missing Lite header")
						}
						completed := `{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","namespace":"web","name":"run","call_id":"call-search","arguments":"{\"search_query\":[{\"q\":\"test\"}]}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
						if strings.HasPrefix(transport, "ws") {
							upgrader := websocket.Upgrader{}
							conn, err := upgrader.Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func() { _ = conn.Close() }()
							_, body, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							captured <- body
							_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
							return
						}
						body, _ := io.ReadAll(r.Body)
						captured <- body
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}))
					defer server.Close()
					cfg := &config.Config{}
					auth := &cliproxyauth.Auth{Provider: "codex", ID: t.Name(), Attributes: map[string]string{"api_key": "key", "base_url": server.URL}}
					req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"search"}],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false,"tools":[{"type":"web_search_preview"},{"type":"image_generation"}]}`)}
					req.Payload, _ = sjson.SetBytes(req.Payload, "model", model)
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {"true"}}}
					switch intent {
					case "body-mirror":
						opts.Headers = nil
						req.Payload, _ = sjson.SetBytes(req.Payload, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite", "true")
					case "official-without-header":
						opts.Headers = nil
						req.Payload, _ = sjson.SetBytes(req.Payload, "client_metadata.x-codex-turn-metadata", `{"session_id":"session","turn_id":"turn"}`)
					case "configured":
						opts.Headers = nil
						auth.Attributes["header:"+codexResponsesLiteHeader] = "true"
					}
					httpExec := NewCodexExecutor(cfg)
					execute := httpExec.Execute
					stream := httpExec.ExecuteStream
					if strings.HasPrefix(transport, "ws") {
						ws := NewCodexWebsocketsExecutor(cfg)
						execute = ws.Execute
						stream = ws.ExecuteStream
					}
					if strings.HasSuffix(transport, "stream") {
						result, err := stream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := execute(context.Background(), auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					body := <-captured
					if gjson.GetBytes(body, "tools").Exists() || util.ClassifyCodexResponsesLiteTools(body) != util.CodexResponsesLiteToolsCompatible {
						t.Fatalf("tools: %s", body)
					}
					descriptors := util.CollectResponsesToolWinners(gjson.ParseBytes(body))
					for _, name := range []string{"image_gen__imagegen", "web__run"} {
						if _, ok := descriptors[name]; !ok {
							t.Fatalf("missing %s: %s", name, body)
						}
					}
					assertCodexReservedWireFixtures(t, body)
					if strings.HasPrefix(transport, "ws") && gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String() != "true" {
						t.Fatal("missing per-request mirror")
					}
				})
			}
		}
	}
}

func TestCodex156LitePolicyMatrix(t *testing.T) {
	base := []byte(`{"model":"gpt-5.6-sol","reasoning":{"context":"all_turns"},"parallel_tool_calls":false,"input":[],"tools":[{"type":"image_generation"},{"type":"web_search"}]}`)
	headers := http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {"true"}}
	for _, mode := range []config.DisableImageGenerationMode{config.DisableImageGenerationOff, config.DisableImageGenerationAll, config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough} {
		cfg := &config.Config{}
		cfg.DisableImageGeneration = mode
		got := mustApplyCodexImagePolicy(t, base, "gpt-5.6-sol", nil, cfg, headers, "/v1/responses", false)
		if mode == config.DisableImageGenerationPassthrough {
			if string(got) != string(base) {
				t.Fatal("passthrough changed")
			}
			continue
		}
		descriptors := util.CollectResponsesToolWinners(gjson.ParseBytes(got))
		_, image := descriptors["image_gen__imagegen"]
		_, web := descriptors["web__run"]
		if image != (mode == config.DisableImageGenerationOff) || !web {
			t.Fatalf("mode=%s: %s", mode, got)
		}
	}
	cfg := &config.Config{}
	if got := mustApplyCodexImagePolicy(t, base, "gpt-5.6-sol", nil, cfg, nil, "/v1/responses", true); !helps.HasCodexImageTool(got) || util.ClassifyCodexResponsesLiteTools(got) != util.CodexResponsesLiteToolsCompatible {
		t.Fatalf("official Lite request not normalized: %s", got)
	}
	for _, parallel := range []any{true, "false", nil, 0} {
		body, _ := sjson.SetBytes(base, "parallel_tool_calls", parallel)
		h := make(http.Header)
		ensureCodexResponsesLiteHeader(h, body, "gpt-5.6-sol", true)
		if h.Get(codexResponsesLiteHeader) != "" {
			t.Fatal("invalid parallel gained header")
		}
	}
	for _, model := range []string{"gpt-5.5", "unknown-model", "gpt-5.6-sol"} {
		h := make(http.Header)
		ensureCodexResponsesLiteHeader(h, base, model, true)
		if h.Get(codexResponsesLiteHeader) != "" {
			t.Fatal("hosted tools gained auto Lite header")
		}
	}
}

func TestCodex156ErrorCodesAcrossTransports(t *testing.T) {
	for _, code := range []string{"slow_down", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "bio_policy"} {
		body := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"original message"}}`, code))
		want := 429
		if code == "bio_policy" {
			want = 400
		}
		httpErr := newCodexStatusErr(400, body)
		sseErr, _, ok := codexTerminalFailureErr([]byte(fmt.Sprintf(`{"type":"response.failed","response":%s}`, body)))
		wsErr, wsOK := parseCodexWebsocketError([]byte(fmt.Sprintf(`{"type":"error","status":400,"error":{"code":%q,"message":"original message"}}`, code)))
		if !ok || !wsOK {
			t.Fatal("missing terminal error")
		}
		for _, err := range []error{httpErr, sseErr, wsErr} {
			status := err.(interface{ StatusCode() int }).StatusCode()
			if status != want || gjson.Get(err.Error(), "error.code").String() != code {
				t.Fatalf("%s: %v", code, err)
			}
			if code == "bio_policy" && !err.(cliproxyexecutor.RequestScopedError).IsRequestScoped() {
				t.Fatal("policy error eligible for account retry")
			}
		}
	}
}

func TestCodex156ImagesGenerationID(t *testing.T) {
	for _, generation := range []string{"", "gen-123"} {
		raw := []byte(`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"AA==","imagegen_request_id":"different-request"}]}}`)
		if generation != "" {
			raw, _ = sjson.SetBytes(raw, "response.output.0.generation_id", generation)
		}
		results, created, usage, meta, err := codexExtractImageResults(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := codexBuildImagesAPIResponse(results, created, usage, meta, "b64_json")
		if err != nil {
			t.Fatal(err)
		}
		value := gjson.GetBytes(out, "data.0.generation_id")
		if value.String() != generation || value.Exists() != (generation != "") {
			t.Fatalf("generation id: %s", out)
		}
		frame := codexBuildImageCompletedFrame(results[0], nil, "b64_json", "image_generation")
		if (strings.Contains(string(frame), `"generation_id"`)) != (generation != "") {
			t.Fatalf("frame %s", frame)
		}
		partial := []byte(`{"partial_image_b64":"AA==","imagegen_request_id":"different-request"}`)
		if generation != "" {
			partial, _ = sjson.SetBytes(partial, "generation_id", generation)
		}
		frame = codexBuildImagePartialFrame(partial, "b64_json", "image_generation")
		if strings.Contains(string(frame), `"generation_id"`) != (generation != "") {
			t.Fatalf("partial frame %s", frame)
		}
	}
}

func TestCodex156HeaderProfilePrecedence(t *testing.T) {
	for _, disableCloaking := range []bool{false, true} {
		cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: disableCloaking}}
		auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token", "codex_client_system": "windows"}}
		for _, configured := range []bool{false, true} {
			if configured {
				cfg.CodexHeaderDefaults.UserAgent = "configured-ua"
				auth.Attributes = map[string]string{"header:Originator": "configured-origin"}
			}
			client := http.Header{"User-Agent": {"codex-tui/0.156.0 (Linux; x86_64)"}, "Originator": {"codex_exec"}}
			req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			applyCodexHeaders(req, auth, "token", true, cfg, client)
			ws := applyCodexWebsocketHeaders(context.Background(), nil, auth, "token", cfg, true, client)
			wantUA, wantOrigin := client.Get("User-Agent"), "codex_exec"
			if configured {
				wantUA, wantOrigin = "configured-ua", "configured-origin"
			}
			for _, headers := range []http.Header{req.Header, ws} {
				if headers.Get("User-Agent") != wantUA || headers.Get("Originator") != wantOrigin {
					t.Fatalf("cloaking disabled=%v configured=%v: %v", disableCloaking, configured, headers)
				}
			}
		}
	}
}

func TestCodex156FinalCacheAffinitySurvivesCacheHelpers(t *testing.T) {
	original := []byte(`{"prompt_cache_key":"client-cache","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"real-session\"}"}}`)
	body, _ := sjson.SetBytes(original, "prompt_cache_key", "configured-cache")
	body, id, _ := helps.ApplyCodexOAuthFidelity(body, "account", "mac", false)
	req := cliproxyexecutor.Request{Payload: original}
	e := NewCodexExecutor(&config.Config{})
	httpReq, httpBody, _, err := e.cacheHelper(context.Background(), sdktranslator.FormatOpenAIResponse, "https://chatgpt.com/backend-api/codex/responses", nil, req, original, body)
	if err != nil {
		t.Fatal(err)
	}
	wsBody, wsHeaders := applyCodexPromptCacheHeaders(sdktranslator.FormatOpenAIResponse, req, body)
	for _, upstream := range []struct {
		body    []byte
		headers http.Header
	}{{httpBody, httpReq.Header}, {wsBody, wsHeaders}} {
		helps.ApplyCodexOAuthHeaders(upstream.headers, id, "model", true, "", "")
		if gjson.GetBytes(upstream.body, "prompt_cache_key").String() != "configured-cache" || upstream.headers.Get("Session-Id") != "configured-cache" || id.SessionID != "real-session" {
			t.Fatalf("final cache affinity changed: %s %v", upstream.body, upstream.headers)
		}
	}
}

func TestCodex156WebsocketCredentialRevisionReconnects(t *testing.T) {
	captured := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		captured <- r.Header.Get("Authorization")
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	executor := NewCodexWebsocketsExecutor(&config.Config{})
	executor.store = &codexWebsocketSessionStore{sessions: map[string]*codexWebsocketSession{}}
	sess := executor.getOrCreateSession(t.Name())
	defer executor.CloseExecutionSession(t.Name())
	auth := &cliproxyauth.Auth{ID: "stable-id", Provider: "codex", Metadata: map[string]any{"access_token": "old", "account_id": "account"}}
	target := "ws" + strings.TrimPrefix(server.URL, "http")
	headers := http.Header{"Authorization": {"Bearer old"}}
	first, _, _, err := executor.ensureUpstreamConn(context.Background(), auth, sess, auth.ID, target, headers)
	if err != nil {
		t.Fatal(err)
	}
	if <-captured != "Bearer old" {
		t.Fatal("wrong first credential")
	}
	headers.Set("Cookie", "__oailb=new")
	headers.Set("X-Codex-Turn-State", "new-turn")
	reused, _, _, err := executor.ensureUpstreamConn(context.Background(), auth, sess, auth.ID, target, headers)
	if err != nil || reused != first {
		t.Fatal("per-turn changes reconnected")
	}
	headers.Set("Authorization", "Bearer renewed")
	auth.Metadata["access_token"] = "renewed"
	if conn, _ := existingWebsocketSessionConn(sess, auth.ID, target, helps.CodexConnectionFingerprint(auth, headers, "")); conn != nil {
		t.Fatal("required reuse accepted old credential")
	}
	renewed, _, _, err := executor.ensureUpstreamConn(context.Background(), auth, sess, auth.ID, target, headers)
	if err != nil || renewed == first {
		t.Fatal("refresh failed to reconnect", err)
	}
	if <-captured != "Bearer renewed" {
		t.Fatal("wrong refreshed credential")
	}
}

func TestCodex156OAuthFinalIdentityAcrossTransports(t *testing.T) {
	for _, transport := range []string{"http", "http-stream", "ws", "ws-stream", "compact"} {
		t.Run(transport, func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, want := range map[string]string{"Session-Id": "cache", "Thread-Id": "thread", "X-Client-Request-Id": "thread", "X-Codex-Window-Id": "thread:0", "Version": "0.156.0", "X-Codex-Beta-Features": "extra,remote_compaction_v2"} {
					if got := r.Header.Get(key); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
				if !strings.Contains(r.UserAgent(), "Windows") || !strings.Contains(r.UserAgent(), "0.156.0") {
					t.Error(r.UserAgent())
				}
				completed := `{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
				if strings.HasPrefix(transport, "ws") {
					upgrader := websocket.Upgrader{}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, body, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					captured <- body
					_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.Header.Get("Content-Encoding") == "zstd" {
					decoder, err := zstd.NewReader(nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer decoder.Close()
					body, err = decoder.DecodeAll(body, nil)
					if err != nil {
						t.Error(err)
						return
					}
				}
				captured <- body
				if transport == "compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"r","object":"response.compaction","output":[]}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
			}))
			defer server.Close()
			cfg := &config.Config{}
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "oauth-token", "account_id": "owner", "codex_client_system": "windows"}}
			defer helps.InvalidateCodexCookieJar(auth.ID)
			defer helps.InvalidateCodexTurnStates(auth.ID)
			body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"cache","reasoning":{"context":"all_turns"},"parallel_tool_calls":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"turn\",\"analytics_enabled\":false,\"compaction\":{\"phase\":\"pre\"}}"},"input":[{"type":"compaction_trigger"},{"type":"additional_tools","id":"at-client","role":"developer","tools":[{"type":"function","name":"exec","parameters":{}}]}]}`)
			req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: body}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"X-Codex-Beta-Features": {"extra"}}}
			httpExec := NewCodexExecutor(cfg)
			execute := httpExec.Execute
			stream := httpExec.ExecuteStream
			if strings.HasPrefix(transport, "ws") {
				ws := NewCodexWebsocketsExecutor(cfg)
				execute = ws.Execute
				stream = ws.ExecuteStream
			}
			if transport == "compact" {
				opts.Alt = "responses/compact"
			}
			if strings.HasSuffix(transport, "stream") {
				result, err := stream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			got := <-captured
			metadata := gjson.Parse(gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String())
			if metadata.Get("session_id").String() != "session" || gjson.GetBytes(got, "prompt_cache_key").String() != "cache" || metadata.Get("analytics_enabled").Type != gjson.False || metadata.Get("compaction.phase").String() != "pre" {
				t.Fatalf("identity: %s", got)
			}
			foundCompaction, foundTools := false, false
			for _, item := range gjson.GetBytes(got, "input").Array() {
				foundCompaction = foundCompaction || item.Get("type").String() == "compaction_trigger"
				foundTools = foundTools || item.Get("id").String() == "at-client" && item.Get("tools.0.name").String() == "exec"
			}
			if !foundCompaction || !foundTools {
				t.Fatalf("native items changed: %s", got)
			}
			if helps.HasCodexImageTool(got) != (transport != "compact") {
				t.Fatalf("incorrect image injection for %s: %s", transport, got)
			}
		})
	}
}

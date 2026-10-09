package executor

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func runtimeImageAuth(t *testing.T, cfg *config.Config, auth *coreauth.Auth) {
	t.Helper()
	cfg.AuthDir = t.TempDir()
	path := filepath.Join(cfg.AuthDir, auth.ID)
	auth.Attributes[coreauth.AttributePath] = path
	auth.Metadata["access_token"] = "synthetic-worker-token"
	if err := codexshared.Write(path, auth.Metadata); err != nil {
		t.Fatal(err)
	}
}

func runtimeImagePayload(t *testing.T, multipartEdit bool) ([]byte, string) {
	t.Helper()
	if !multipartEdit {
		return []byte(`{"model":"gpt-image-1.5","prompt":"original","quality":"low","images":[{"image_url":"data:image/png;base64,AQID"}],"mask":{"image_url":"data:image/png;base64,BAUG"}}`), "application/json"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, field := range []struct{ key, value string }{{"model", "gpt-image-1.5"}, {"prompt", "original"}, {"quality", "low"}, {"n", "2"}, {"extra", "one"}, {"extra", "two"}} {
		if err := w.WriteField(field.key, field.value); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"image[]", []byte{1, 2, 3}}, {"image[]", []byte{7, 8, 9}}, {"mask", []byte{4, 5, 6}}} {
		part, err := w.CreateFormFile(file.name, "original.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func runtimeImageComplete(c *websocket.Conn, req bridge.Request) {
	_ = c.WriteJSON(map[string]any{"method": "cpa/inference/completed", "params": map[string]any{"requestId": req.RequestID}})
}

func runtimeImageResult(t *testing.T, auto *CodexAutoExecutor, ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, stream bool) ([]byte, error) {
	t.Helper()
	if !stream {
		resp, err := auto.Execute(ctx, auth, req, opts)
		return resp.Payload, err
	}
	result, err := auto.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for chunk := range result.Chunks {
		buf.Write(chunk.Payload)
		if chunk.Err != nil {
			return buf.Bytes(), chunk.Err
		}
	}
	return buf.Bytes(), nil
}

func TestCodexRuntimeImagesRouteAndFinalPayload(t *testing.T) {
	for _, path := range []string{codexImagesGenerationsPath, codexImagesEditsPath} {
		for _, stream := range []bool{false, true} {
			for _, form := range []bool{false, true} {
				if form && path != codexImagesEditsPath {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/multipart=%t", path, stream, form), func(t *testing.T) {
					var nativeCalls, masterCalls atomic.Int32
					settings, auth := runtimeTestMaster(t, "images.json", func(c *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
						masterCalls.Add(1)
						if req.Operation != strings.TrimPrefix(path, "/v1/") || req.ImageAPI != "images" || req.RequestID != "image-correlation" {
							t.Errorf("wrong operation or request correlation: %+v", req)
						}
						if gjson.GetBytes(req.Request, "prompt").String() != "configured" || gjson.GetBytes(req.Request, "quality").Exists() || gjson.GetBytes(req.Request, "model").String() != "gpt-image-2.5" || gjson.GetBytes(req.Request, "extension.nested").Int() != 42 {
							t.Errorf("final payload rules/model mapping lost: %s", req.Request)
						}
						if form && (gjson.GetBytes(req.Request, "images.#").Int() != 2 || !strings.HasSuffix(gjson.GetBytes(req.Request, "images.0.image_url").String(), "AQID") || !strings.HasSuffix(gjson.GetBytes(req.Request, "mask.image_url").String(), "BAUG") || gjson.GetBytes(req.Request, "extra.#").Int() != 2) {
							t.Error("multipart images, mask or repeated fields lost")
						}
						contentType := "application/json"
						if stream {
							contentType = "text/event-stream"
						}
						_ = c.WriteJSON(map[string]any{"id": rpc.ID, "result": map[string]any{"requestId": req.RequestID, "statusCode": 200, "headers": http.Header{"Content-Type": {contentType}}}})
						if stream {
							runtimeTestBody(c, req, []byte("event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"partial\"}\n\nevent: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"final\"}\n\n"))
						} else {
							runtimeTestBody(c, req, []byte(`{"created":123,"data":[{"b64_json":"final"}]}`))
						}
						runtimeImageComplete(c, req)
					}, func(caps *bridge.Capabilities) {
						caps.Operations = append(caps.Operations, "images/generations", "images/edits")
					})
					cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
					models := []config.PayloadModelRule{{Name: "gpt-image-2.5", Protocol: "openai"}}
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"prompt": "configured", "extension.nested": 42}}}, Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"quality"}}}}
					runtimeImageAuth(t, cfg, auth)
					payload, contentType := runtimeImagePayload(t, form)
					opts := codexOpenAIImageTestOptions(path, stream)
					opts.Headers = make(http.Header)
					opts.Headers.Set("Content-Type", contentType)
					ctx := logging.WithRequestID(context.Background(), "image-correlation")
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
						nativeCalls.Add(1)
						return nil, fmt.Errorf("CPA must not send upstream")
					})))
					result, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-2.5", Payload: payload}, opts, stream)
					if err != nil || !bytes.Contains(result, []byte("final")) || nativeCalls.Load() != 0 || masterCalls.Load() != 1 {
						t.Fatalf("native=%d master=%d err=%v output=%s", nativeCalls.Load(), masterCalls.Load(), err, result)
					}
					if stream && !bytes.Contains(result, []byte("partial")) {
						t.Fatal("partial image event lost")
					}
				})
			}
		}
	}
}

func TestCodexRuntimeImagesFailuresNeverUseCPAUpstream(t *testing.T) {
	for _, mode := range []string{"master_unavailable", "missing_capability", "worker_unavailable", "upstream_error", "cancel"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				var nativeCalls, masterCalls atomic.Int32
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				settings, auth := runtimeTestMaster(t, "error.json", func(c *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
					masterCalls.Add(1)
					if mode == "cancel" {
						cancel()
						return
					}
					status := 502
					if mode == "upstream_error" {
						status = 429
					}
					_ = c.WriteJSON(map[string]any{"id": rpc.ID, "error": map[string]any{"code": -32000, "message": mode, "data": map[string]any{"httpStatus": status, "body": `{"error":{"message":"controlled failure"}}`}}})
				}, func(caps *bridge.Capabilities) {
					if mode != "missing_capability" {
						caps.Operations = append(caps.Operations, "images/generations", "images/edits")
					}
				})
				if mode == "master_unavailable" {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { masterCalls.Add(1); http.Error(w, "offline", 503) }))
					defer server.Close()
					settings.URL = "ws" + strings.TrimPrefix(server.URL, "http")
				}
				cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
				runtimeImageAuth(t, cfg, auth)
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
					nativeCalls.Add(1)
					return nil, fmt.Errorf("unexpected CPA call")
				})))
				payload, _ := runtimeImagePayload(t, false)
				_, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
				if err == nil || nativeCalls.Load() != 0 {
					t.Fatalf("native=%d master=%d err=%v", nativeCalls.Load(), masterCalls.Load(), err)
				}
				if mode == "missing_capability" && masterCalls.Load() != 0 {
					t.Fatal("unsupported operation reached inference")
				}
			})
		}
	}
}

func TestCodexRuntimeImagesResponsesCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			settings, auth := runtimeTestMaster(t, "compat.json", func(c *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
				if req.ImageAPI != "responses" || req.Operation != "images/generations" || gjson.GetBytes(req.Request, "tools.0.type").String() != "image_generation" {
					t.Error("image Responses conversion lost")
				}
				runtimeTestAccept(c, rpc, req)
				runtimeTestBody(c, req, []byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"part\"}\n\n"))
				runtimeTestFinish(c, req, `{"type":"response.completed","response":{"created_at":1,"output":[{"type":"image_generation_call","result":"final","output_format":"png"}]}}`)
			}, func(caps *bridge.Capabilities) { caps.Operations = append(caps.Operations, "images/generations") })
			cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
			runtimeImageAuth(t, cfg, auth)
			result, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), context.Background(), auth, coreexecutor.Request{Model: "image-alias", Payload: []byte(`{"model":"image-alias","prompt":"draw"}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream), stream)
			if err != nil || !bytes.Contains(result, []byte("final")) {
				t.Fatalf("%s %v", result, err)
			}
		})
	}
}

func TestCodexRuntimeImagesPayloadBeforeWorkerNormalization(t *testing.T) {
	settings, auth := runtimeTestMaster(t, "normalize.json", func(c *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
		if req.ImageAPI != "responses" || req.Operation != "images/generations" {
			t.Error("image request did not use the Responses worker builder")
		}
		for _, field := range []string{"input.0.type", "instructions", "store", "parallel_tool_calls"} {
			if gjson.GetBytes(req.Request, field).Exists() {
				t.Errorf("CPA did not apply its payload filter before handing off %s", field)
			}
		}
		if gjson.GetBytes(req.Request, "input.0.content").String() != "draw" || gjson.GetBytes(req.Request, "stream").Type != gjson.False {
			t.Errorf("CPA payload overrides were not passed to the worker: %s", req.Request)
		}
		runtimeTestAccept(c, rpc, req)
		runtimeTestFinish(c, req, `{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"final","output_format":"png"}]}}`)
	}, func(caps *bridge.Capabilities) { caps.Operations = append(caps.Operations, "images/generations") })
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
	models := []config.PayloadModelRule{{Name: "gpt-5.4-mini", Protocol: "codex"}}
	cfg.Payload = config.PayloadConfig{
		Override: []config.PayloadRule{{Models: models, Params: map[string]any{"input.0.content": "draw", "stream": false}}},
		Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"input.0.type", "instructions", "store", "parallel_tool_calls"}}},
	}
	runtimeImageAuth(t, cfg, auth)
	result, err := NewCodexAutoExecutor(cfg).Execute(context.Background(), auth, coreexecutor.Request{Model: "image-alias", Payload: []byte(`{"model":"image-alias","prompt":"draw"}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, false))
	if err != nil || !bytes.Contains(result.Payload, []byte("final")) {
		t.Fatalf("image handoff failed: %s %v", result.Payload, err)
	}
}

func TestCodexRuntimeImagesPartialThenError(t *testing.T) {
	for _, failure := range []string{"event_error", "transfer_error", "incomplete"} {
		t.Run(failure, func(t *testing.T) {
			var nativeCalls atomic.Int32
			settings, auth := runtimeTestMaster(t, "partial.json", func(c *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
				runtimeTestAccept(c, rpc, req)
				runtimeTestBody(c, req, []byte("data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"part\"}\n\n"))
				switch failure {
				case "event_error":
					runtimeTestBody(c, req, []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"image failed\",\"type\":\"server_error\"}}\n\n"))
					runtimeImageComplete(c, req)
				case "transfer_error":
					_ = c.WriteJSON(map[string]any{"method": "cpa/inference/error", "params": map[string]any{"requestId": req.RequestID, "httpStatus": 502, "message": "image transfer truncated"}})
				case "incomplete":
					runtimeImageComplete(c, req)
				}
			}, func(caps *bridge.Capabilities) { caps.Operations = append(caps.Operations, "images/generations") })
			cfg := &config.Config{Codex: config.CodexConfig{Runtime: settings}}
			runtimeImageAuth(t, cfg, auth)
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
				nativeCalls.Add(1)
				return nil, fmt.Errorf("CPA upstream forbidden")
			})))
			payload, _ := runtimeImagePayload(t, false)
			output, err := runtimeImageResult(t, NewCodexAutoExecutor(cfg), ctx, auth, coreexecutor.Request{Model: "gpt-image-1.5", Payload: payload}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, true), true)
			if err == nil || !bytes.Contains(output, []byte("partial_image")) || nativeCalls.Load() != 0 {
				t.Fatalf("partial/error lost or direct call made: output=%s error=%v native=%d", output, err, nativeCalls.Load())
			}
		})
	}
}

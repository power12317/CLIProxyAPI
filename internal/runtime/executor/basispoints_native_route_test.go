package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsNativeRouteBeforeNetworkAndRetainsSession(t *testing.T) {
	for _, mode := range []string{"http", "forced_ws", "downstream_ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				var bpsDials, bpsFrames, nativeHTTP, nativeWS atomic.Int32
				completed := []byte("{\"type\":\"response.completed\",\"basispoints_replay_cursor\":0,\"response\":{\"id\":\"route-result\",\"status\":\"completed\",\"output\":[]}}")
				tools := []any{map[string]any{"type": "tool_search", "execution": "client"}, map[string]any{"type": "web_search", "external_web_access": true}}
				wantTools, _ := json.Marshal(tools)
				checkNative := func(body []byte) {
					if gjson.GetBytes(body, "tools").Raw != string(wantTools) || !strings.Contains(string(body), "data:image/png;base64,") {
						t.Error("native route altered tool declarations or inline image")
					}
				}
				bpsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					bpsDials.Add(1)
					defer func() { _ = conn.Close() }()
					for {
						_, _, errRead := conn.ReadMessage()
						if errRead != nil {
							return
						}
						bpsFrames.Add(1)
						if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
							return
						}
					}
				}))
				defer bpsServer.Close()
				nativeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					nativeWS.Add(1)
					if mode == "forced_ws" {
						if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
							t.Error("forced WS dialed the wrong upstream")
						}
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
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
					checkNative(body)
					_ = conn.WriteMessage(websocket.TextMessage, completed)
					_, _, _ = conn.ReadMessage()
				}))
				defer nativeServer.Close()
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough}, Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: mode == "forced_ws"}}
				exec := NewCodexAutoExecutor(cfg)
				exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
				isolateBasispointsWebsockets(t, exec.basispointsExec)
				exec.basispointsExec.websocketDial = basispointsTestDial(bpsServer.URL)
				if mode == "forced_ws" {
					wsCfg := *cfg
					wsCfg.ProxyURL = nativeServer.URL
					exec.wsExec = NewCodexWebsocketsExecutor(&wsCfg)
				}
				auth := basispointsTestAuth()
				auth.ID = t.Name()
				if mode == "downstream_ws" {
					auth.Attributes = map[string]string{"base_url": nativeServer.URL, "websockets": "true"}
				}
				t.Cleanup(func() { exec.CloseExecutionSession(t.Name()) })
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/codex/responses" {
						return nil, fmt.Errorf("unexpected upstream request: %s", r.URL)
					}
					nativeHTTP.Add(1)
					body, _ := io.ReadAll(r.Body)
					checkNative(body)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(completed) + "\n\n")), Request: r}, nil
				})))
				opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Headers: http.Header{"Session_id": {t.Name()}}, Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name(), core.CallerScopeMetadataKey: t.Name(), core.CanonicalSessionIDMetadataKey: t.Name()}}
				invoke := func(requestCtx context.Context, payload map[string]any) {
					t.Helper()
					raw, _ := json.Marshal(payload)
					req := core.Request{Model: "gpt-6-astra", Payload: raw}
					if stream {
						result, err := exec.ExecuteStream(requestCtx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := exec.Execute(requestCtx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				plain := map[string]any{"model": "gpt-6-astra", "input": "hello"}
				invoke(core.WithDownstreamWebsocket(ctx), plain)
				requestCtx := ctx
				if mode == "downstream_ws" {
					requestCtx = core.WithDownstreamWebsocket(ctx)
				}
				invoke(requestCtx, map[string]any{"model": "gpt-6-astra", "tool_choice": map[string]any{"type": "web_search"}, "tools": tools, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="}}}}})
				if bpsDials.Load() != 1 || bpsFrames.Load() != 1 {
					t.Fatal("native route contacted Basispoints")
				}
				invoke(core.WithDownstreamWebsocket(ctx), plain)
				if bpsDials.Load() != 1 || bpsFrames.Load() != 2 {
					t.Fatal("native route closed the retained Basispoints connection")
				}
				wantHTTP, wantWS := int32(1), int32(0)
				if mode == "forced_ws" {
					wantWS = 5
				}
				if mode == "downstream_ws" {
					wantHTTP, wantWS = 0, 1
				}
				if nativeHTTP.Load() != wantHTTP || nativeWS.Load() != wantWS {
					t.Fatalf("native HTTP=%d WS=%d", nativeHTTP.Load(), nativeWS.Load())
				}
			})
		}
	}
}

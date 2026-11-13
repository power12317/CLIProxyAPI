package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBasispointsRequiresIndependentOfficeGrant(t *testing.T) {
	auth := basispointsTestAuth()
	delete(auth.Metadata, "basispoints")
	auth.Metadata["siwc"] = map[string]any{"access_token": "siwc-cannot-be-used"}
	auth.Metadata["use_siwc"] = true
	cfg := forceWebsocketTestConfig()
	cfg.Codex.Basispoints.Enabled = true
	exec := NewCodexAutoExecutor(cfg)
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unexpected network request")
	})))
	_, err := exec.Execute(ctx, auth, core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
	if err == nil || !strings.Contains(err.Error(), "basispoints_authorization_required") || calls != 0 {
		t.Fatalf("missing Office grant did not stop generation: calls=%d err=%v", calls, err)
	}
}

func TestBasispointsMissingGrantDoesNotDisableCodexAccount(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.Basispoints.Enabled = true
	exec := NewCodexAutoExecutor(cfg)
	auth := basispointsTestAuth()
	auth.ID = t.Name()
	auth.Status = coreauth.StatusActive
	auth.Metadata["use_basispoints"] = true
	delete(auth.Metadata, "basispoints")
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "office-isolation-test"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	_, err := manager.Execute(t.Context(), []string{"codex"}, core.Request{Model: "office-isolation-test", Payload: []byte(`{"input":"hello"}`)}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
	if err == nil || !strings.Contains(err.Error(), "basispoints_authorization_required") {
		t.Fatalf("wrong Office authorization failure: %v", err)
	}
	after, _ := manager.GetByID(auth.ID)
	if after.Disabled || after.Unavailable || after.LastError != nil || after.Metadata["access_token"] != auth.Metadata["access_token"] {
		t.Fatal("missing Office grant poisoned the ordinary Codex account")
	}
}

func TestBasispointsAccountSwitchPrecedesCodexRuntime(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			auth := basispointsTestAuth()
			auth.Metadata["use_basispoints"] = true
			auth.Metadata["use_siwc"] = true
			auth.Metadata["siwc"] = map[string]any{"access_token": "siwc-token"}
			auth.Metadata["codex_cli"] = map[string]any{"enabled": true}
			cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, Runtime: config.CodexRuntimeConfig{Enabled: true, URL: "ws://invalid.local"}}}
			exec := NewCodexAutoExecutor(cfg)
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "bps.openai.com" || r.Header.Get("Authorization") != "Bearer "+coreauth.BasispointsGrant(auth)["access_token"].(string) {
					t.Fatal("Office request used wrong endpoint or credential")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
			})))
			req := core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}
			opts := core.Options{SourceFormat: translator.FormatOpenAIResponse}
			if stream {
				result, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("Office calls=%d", calls)
			}
		})
	}
}

func TestBasispointsPayloadRulesAreFinalForBothTransports(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprint(ws), func(t *testing.T) {
			cfg := forceWebsocketTestConfig()
			cfg.Codex.ForceWebsocket = ws
			cfg.CodexHeaderDefaults.FastMode = "fast"
			cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "codex"}}, Params: map[string]any{"service_tier": "configured-tier", "metadata.bps_tools_version_id": "configured-tools", "reasoning_effort": "configured-effort"}}}, Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "codex"}}, Params: []string{"model_selection"}}}}
			exec := NewBasispointsExecutor(cfg)
			isolateBasispointsWebsockets(t, exec)
			check := func(raw []byte) {
				if ojson.GetBytes(raw, "service_tier").String() != "configured-tier" || ojson.GetBytes(raw, "metadata.bps_tools_version_id").String() != "configured-tools" || ojson.GetBytes(raw, "reasoning_effort").String() != "configured-effort" || ojson.GetBytes(raw, "model_selection").Exists() {
					t.Fatalf("built-in transformation overwrote payload rules: %s", raw)
				}
			}
			if ws {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"responses"}}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, raw, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					check(raw)
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`))
				}))
				defer server.Close()
				exec.websocketDial = basispointsTestDial(server.URL)
			}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				check(raw)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
			})))
			_, err := exec.Execute(ctx, basispointsTestAuth(), core.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBasispointsRequiresBothSwitchesForRouting(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, account := range []string{"missing", "off", "on"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("global=%t/account=%s/stream=%t", global, account, stream), func(t *testing.T) {
					cfg := &config.Config{}
					cfg.Codex.Basispoints.Enabled = global
					auth := basispointsTestAuth()
					switch account {
					case "missing":
						delete(auth.Metadata, "use_basispoints")
					case "off":
						auth.Metadata["use_basispoints"] = false
					}
					exec := NewCodexAutoExecutor(cfg)
					wantHost, wantToken := "chatgpt.com", auth.Metadata["access_token"].(string)
					if global && account == "on" {
						wantHost, wantToken = "bps.openai.com", coreauth.BasispointsGrant(auth)["access_token"].(string)
					}
					calls := 0
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						if r.URL.Host != wantHost || r.Header.Get("Authorization") != "Bearer "+wantToken {
							t.Errorf("upstream=%s; expected host %s and selected credential partition", r.URL, wantHost)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"runtime-model\",\"status\":\"completed\",\"output\":[]}}\n\n")), Request: r}, nil
					})))
					req := core.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":"hello"}`)}
					opts := core.Options{SourceFormat: translator.FormatOpenAIResponse}
					if stream {
						result, err := exec.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatalf("upstream calls=%d, want 1", calls)
					}
				})
			}
		}
	}
}

func TestBasispointsGlobalOffRoutesOptedInAccountToCodexRuntime(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			runtimeCalls := make(chan struct{}, 1)
			settings, auth := runtimeTestMaster(t, "office-account.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
				runtimeCalls <- struct{}{}
				runtimeTestAccept(conn, rpc, req)
				runtimeTestFinish(conn, req, runtimeTerminal)
			})
			auth.Metadata["account_id"] = "account"
			auth.Metadata["access_token"] = "ordinary-token"
			withBasispointsTestGrant(auth)
			dir := t.TempDir()
			auth.Attributes[coreauth.AttributePath] = filepath.Join(dir, auth.ID)
			if err := codexshared.Write(auth.Attributes[coreauth.AttributePath], auth.Metadata); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: settings}}
			exec := NewCodexAutoExecutor(cfg)
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				t.Errorf("request bypassed Codex runtime: %s", r.URL)
				return nil, fmt.Errorf("unexpected direct upstream request")
			})))
			req := core.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":"hello"}`)}
			if stream {
				result, err := exec.ExecuteStream(ctx, auth, req, runtimeTestOptions())
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := exec.Execute(ctx, auth, req, runtimeTestOptions()); err != nil {
				t.Fatal(err)
			}
			if len(runtimeCalls) != 1 {
				t.Fatal("request did not reach Codex runtime")
			}
			saved, err := codexshared.Read(auth.Attributes[coreauth.AttributePath])
			if err != nil {
				t.Fatal(err)
			}
			if saved["use_basispoints"] != true || saved["basispoints"] == nil {
				t.Fatal("global disable erased the account preference or Office grant")
			}
		})
	}
}

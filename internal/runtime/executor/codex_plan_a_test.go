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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexPlanAOutboundHeaders(t *testing.T) {
	const model = "gpt-5.6-luna"
	const clientUA = "codex-tui/0.153.4 (Mac OS 15.3.1; arm64) Apple_Terminal/455.1"
	for _, transport := range []string{"http", "http-stream", "ws", "ws-stream", "compact"} {
		for _, tc := range []struct {
			name, system, tier, overrideTier, wantTier    string
			clientUA, configuredUA, credentialUA, modelUA string
			hintOverride, modelHint                       string
			compat, apiKey                                bool
		}{
			{name: "mac_default", system: "mac"},
			{name: "windows_priority", system: "windows", tier: "priority", wantTier: "priority"},
			{name: "ultrafast", tier: "ultrafast", wantTier: "ultrafast"},
			{name: "explicit_default", tier: "default"},
			{name: "explicit_auto", tier: "auto"},
			{name: "fast_alias", tier: "fast", wantTier: "priority"},
			{name: "compat_oauth", tier: "priority", wantTier: "priority", compat: true},
			{name: "api_key", tier: "priority", wantTier: "priority", apiKey: true},
			{name: "client_identity", clientUA: clientUA},
			{name: "desktop_identity", clientUA: "Codex Desktop/0.155.0 (Mac OS; arm64) unknown"},
			{name: "exec_identity", clientUA: "codex_exec/0.156.1 (Linux; x86_64) dumb"},
			{name: "configured_identity", clientUA: clientUA, configuredUA: "configured-ua"},
			{name: "credential_override", configuredUA: "configured-ua", credentialUA: "credential-ua", hintOverride: "custom-route"},
			{name: "model_override", credentialUA: "credential-ua", modelUA: "model-ua", hintOverride: "custom-route", modelHint: "model-route"},
			{name: "final_payload_tier", tier: "priority", overrideTier: "ultrafast", wantTier: "ultrafast"},
			{name: "final_payload_default_omitted", tier: "priority", overrideTier: "default"},
			{name: "final_payload_auto_omitted", overrideTier: "auto"},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				type capture struct {
					headers http.Header
					body    []byte
				}
				captured := make(chan capture, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
						captured <- capture{r.Header.Clone(), body}
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
					captured <- capture{r.Header.Clone(), body}
					if transport == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"id":"r","object":"response.compaction","output":[]}`))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
				}))
				defer server.Close()
				cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: tc.configuredUA}}
				if tc.overrideTier != "" {
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model}}, Params: map[string]any{"service_tier": tc.overrideTier}}}
				}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "oauth-token", "account_id": "owner", "codex_client_system": tc.system}}
				if tc.apiKey {
					auth.Attributes["api_key"] = "test-key"
					auth.Metadata = nil
				}
				if tc.credentialUA != "" {
					auth.Attributes["header:User-Agent"] = tc.credentialUA
				}
				if tc.hintOverride != "" {
					auth.Attributes["header:X-Codex-Routing-Hint"] = tc.hintOverride
				}
				defer helps.InvalidateCodexCookieJar(auth.ID)
				defer helps.InvalidateCodexTurnStates(auth.ID)
				if tc.modelUA != "" {
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(t.Name(), "codex", []*registry.ModelInfo{{ID: model, Config: &registry.ModelConfig{OverrideHeader: map[string]string{"User-Agent": tc.modelUA, "X-Codex-Routing-Hint": tc.modelHint}}}})
					defer reg.UnregisterClient(t.Name())
				}
				body := []byte(`{"model":"stale-client-alias","input":[{"role":"user","content":"hello"}],"client_metadata":{"x-codex-turn-metadata":"{}"}}`)
				if tc.compat {
					body, _ = sjson.DeleteBytes(body, "client_metadata")
				}
				if tc.tier != "" {
					body, _ = sjson.SetBytes(body, "service_tier", tc.tier)
				}
				headers := make(http.Header)
				if tc.clientUA != "" {
					headers.Set("User-Agent", tc.clientUA)
					headers.Set("Version", "0.153.4-client")
				}
				req := cliproxyexecutor.Request{Model: model, Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: headers}
				if tc.compat {
					opts.SourceFormat = sdktranslator.FormatOpenAIResponse
				}
				executor := NewCodexExecutor(cfg)
				execute, stream := executor.Execute, executor.ExecuteStream
				if strings.HasPrefix(transport, "ws") {
					ws := NewCodexWebsocketsExecutor(cfg)
					execute, stream = ws.Execute, ws.ExecuteStream
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
				} else if _, err := execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				got := <-captured
				if gjson.GetBytes(got.body, "model").String() != model || gjson.GetBytes(got.body, "service_tier").String() != tc.wantTier {
					t.Fatalf("unexpected final model/tier: %s", got.body)
				}
				if tc.wantTier == "" && gjson.GetBytes(got.body, "service_tier").Exists() {
					t.Fatalf("ordinary requests must omit service_tier: %s", got.body)
				}
				wantHint := "model=" + model
				if tc.wantTier != "" {
					wantHint += ";tier=" + tc.wantTier
				}
				if tc.apiKey {
					wantHint = ""
				}
				if tc.hintOverride != "" {
					wantHint = tc.hintOverride
				}
				if tc.modelHint != "" {
					wantHint = tc.modelHint
				}
				if got.headers.Get("X-Codex-Routing-Hint") != wantHint {
					t.Errorf("routing hint = %q, want %q", got.headers.Get("X-Codex-Routing-Hint"), wantHint)
				}
				wantUA := helps.CodexSystemUserAgent(tc.system)
				if tc.apiKey {
					wantUA = helps.CodexDefaultUserAgent
				}
				for _, override := range []string{tc.clientUA, tc.configuredUA, tc.credentialUA, tc.modelUA} {
					if override != "" {
						wantUA = override
					}
				}
				if got.headers.Get("User-Agent") != wantUA {
					t.Errorf("UA = %q, want %q", got.headers.Get("User-Agent"), wantUA)
				}
				if !tc.compat && !tc.apiKey {
					wantVersion := helps.CodexClientVersion
					if tc.clientUA != "" {
						wantVersion = "0.153.4-client"
					}
					if got.headers.Get("Version") != wantVersion {
						t.Errorf("Version = %q, want %q", got.headers.Get("Version"), wantVersion)
					}
				}
			})
		}
	}
}

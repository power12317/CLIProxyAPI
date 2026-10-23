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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexClientSIWCSwitchAcrossTransports(t *testing.T) {
	for _, setting := range []string{"default", "true", "false"} {
		for _, transport := range []string{"http", "http-stream", "compact", "ws", "ws-stream"} {
			t.Run(setting+"/"+transport, func(t *testing.T) {
				useSIWC := setting != "false"
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wantToken := "Bearer outer-token"
					if useSIWC {
						wantToken = "Bearer siwc-token"
					}
					if got := r.Header.Get("Authorization"); got != wantToken {
						t.Errorf("Authorization = %q, want %q", got, wantToken)
					}
					if useSIWC && !strings.HasPrefix(r.UserAgent(), "CLIProxyAPI (") {
						t.Errorf("SIWC User-Agent = %q", r.UserAgent())
					}
					if !useSIWC && r.Header.Get("Originator") != "codex-tui" {
						t.Errorf("ordinary OAuth originator = %q", r.Header.Get("Originator"))
					}
					event := `{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
					if strings.HasPrefix(transport, "ws") {
						upgrader := websocket.Upgrader{}
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Error(err)
							return
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					if transport == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"id":"r","object":"response.compaction","output":[]}`))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}))
				defer upstream.Close()
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"base_url": upstream.URL, "websockets": "true"}, Metadata: map[string]any{
					"type": "codex", "access_token": "outer-token", "account_id": "account", "use_siwc": true,
					"siwc": map[string]any{"access_token": "siwc-token", "client_id": "oaiapp_test"},
				}}
				if setting != "default" {
					auth.Metadata["allow_codex_client_siwc"] = useSIWC
				}
				cfg := &config.Config{}
				httpExecutor := NewCodexExecutor(cfg)
				execute, executeStream := httpExecutor.Execute, httpExecutor.ExecuteStream
				if strings.HasPrefix(transport, "ws") {
					wsExecutor := NewCodexWebsocketsExecutor(cfg)
					execute, executeStream = wsExecutor.Execute, wsExecutor.ExecuteStream
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"User-Agent": {"codex-tui/0.163.0"}}}
				if transport == "compact" {
					opts.Alt = "responses/compact"
				}
				req := cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","instructions":"test","input":[{"role":"user","content":"hello"}]}`)}
				if strings.HasSuffix(transport, "stream") {
					result, err := executeStream(context.Background(), auth, req, opts)
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
				if auth.Metadata["use_siwc"] != true {
					t.Fatal("request changed the saved SIWC preference")
				}
			})
		}
	}
}

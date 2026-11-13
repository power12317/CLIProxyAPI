package executor

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexRuntimeManagerSelectsOrdinaryPartition(t *testing.T) {
	for _, partition := range []string{"fresh", "expired_outer", "expired_siwc"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", partition, stream), func(t *testing.T) {
				calls := make(chan struct{}, 1)
				settings, auth := runtimeTestMaster(t, "runtime-account.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
					calls <- struct{}{}
					runtimeTestAccept(conn, rpc, req)
					runtimeTestFinish(conn, req, runtimeTerminal)
				})
				auth.Metadata["account_id"] = "account"
				auth.Metadata["access_token"] = "ordinary-token"
				auth.Metadata["refresh_token"] = "worker-owned-refresh"
				auth.Metadata["expired"] = "2100-01-01T00:00:00Z"
				withBasispointsTestGrant(auth)
				switch partition {
				case "expired_outer":
					auth.Metadata["expired"] = "2000-01-01T00:00:00Z"
				case "expired_siwc":
					auth.Metadata["use_siwc"] = true
					auth.Metadata["siwc"] = map[string]any{"access_token": "unused-siwc", "refresh_token": "siwc-refresh", "client_id": "siwc-client", "expired": "2000-01-01T00:00:00Z"}
				}
				dir := t.TempDir()
				auth.Attributes[coreauth.AttributePath] = filepath.Join(dir, auth.ID)
				if err := codexshared.Write(auth.Attributes[coreauth.AttributePath], auth.Metadata); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: settings, ForceWebsocket: true}}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
				ctx := coreauth.WithSkipPersist(t.Context())
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "runtime-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
					t.Errorf("request escaped worker: %s", r.URL)
					return nil, fmt.Errorf("unexpected direct upstream")
				})))
				req := core.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":"hello"}`)}
				var errExec error
				if stream {
					var result *core.StreamResult
					result, errExec = manager.ExecuteStream(ctx, []string{"codex"}, req, runtimeTestOptions())
					if errExec == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								errExec = chunk.Err
							}
						}
					}
				} else {
					_, errExec = manager.Execute(ctx, []string{"codex"}, req, runtimeTestOptions())
				}
				if partition == "expired_outer" {
					if errExec == nil || len(calls) != 0 {
						t.Fatalf("expired ordinary credential reached worker: calls=%d err=%v", len(calls), errExec)
					}
					return
				}
				if errExec != nil {
					t.Fatal(errExec)
				}

				if len(calls) != 1 {
					t.Fatal("Codex worker was not selected")
				}
			})
		}
	}
}

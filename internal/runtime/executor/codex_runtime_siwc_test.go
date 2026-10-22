package executor

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexRuntimeUsesOrdinaryPartitionWithoutChangingSIWCPreference(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			settings, auth := runtimeTestMaster(t, "siwc-account.json", func(conn *websocket.Conn, rpc runtimeTestRPC, req bridge.Request) {
				runtimeTestAccept(conn, rpc, req)
				runtimeTestFinish(conn, req, runtimeTerminal)
			})
			auth.Metadata["use_siwc"] = true
			auth.Attributes["use_siwc"] = "true"
			auth.Metadata["access_token"] = "ordinary-token"
			before := auth.Clone()
			e := runtimeTestExecutor(settings)
			req := coreexecutor.Request{Model: "runtime-model", Payload: []byte(`{"model":"runtime-model","input":"hello"}`)}
			if stream {
				result, err := e.ExecuteStream(context.Background(), auth, req, runtimeTestOptions())
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(context.Background(), auth, req, runtimeTestOptions()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(auth.Metadata, before.Metadata) || !reflect.DeepEqual(auth.Attributes, before.Attributes) {
				t.Fatal("runtime request changed the stored SIWC preference")
			}
		})
	}
}

func TestCodexNativePreparationUsesSIWCAfterRuntimeIsDisabled(t *testing.T) {
	dir := t.TempDir()
	auth := runtimeTestAuth("siwc-account.json")
	auth.Attributes[coreauth.AttributePath] = filepath.Join(dir, auth.ID)
	auth.Metadata["access_token"] = "ordinary-token"
	auth.Metadata["use_siwc"] = true
	auth.Metadata["siwc"] = map[string]any{"access_token": "siwc-token"}
	if err := codexshared.Write(auth.Attributes[coreauth.AttributePath], auth.Metadata); err != nil {
		t.Fatal(err)
	}
	executor := NewCodexAutoExecutor(&config.Config{AuthDir: dir})
	for _, test := range []struct{ url, token string }{
		{"https://api.openai.com/v1/responses", "siwc-token"},
		{"https://chatgpt.com/backend-api/codex/alpha/search", "ordinary-token"},
	} {
		request, err := http.NewRequest(http.MethodPost, test.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := executor.PrepareRequest(request, auth); err != nil {
			t.Fatal(err)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+test.token {
			t.Fatalf("authorization=%q, want selected credential partition", got)
		}
	}
}

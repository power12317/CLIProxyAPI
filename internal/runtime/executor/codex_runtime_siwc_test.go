package executor

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/gorilla/websocket"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
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

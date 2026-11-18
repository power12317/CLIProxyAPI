package executor

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexRuntimeKeepsNativeEffortBeforeIPC(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "compact"} {
		for _, effort := range []string{"none", "auto", "ultra", "future-native-effort"} {
			t.Run(mode+"/"+effort, func(t *testing.T) {
				captured := make(chan bridge.Request, 1)
				settings, auth := runtimeTestMaster(t, "native-effort", func(c *websocket.Conn, m runtimeTestRPC, request bridge.Request) {
					captured <- request
					runtimeTestAccept(c, m, request)
					runtimeTestFinish(c, request, runtimeTerminal)
				}, func(caps *bridge.Capabilities) { caps.Operations = append(caps.Operations, "responses/compact") })
				doc := ojson.NewDocString(`{"model":"gpt-6.1-sol","input":[{"type":"configuration_update","reasoning":{"effort":"ultra"}},{"role":"user","content":"hello"}]}`)
				if err := doc.Set("reasoning.effort", effort); err != nil {
					t.Fatal(err)
				}
				req := coreexecutor.Request{Model: "gpt-6.1-sol", Payload: doc.Bytes()}
				opts := runtimeTestOptions()
				e := runtimeTestExecutor(settings)
				if mode == "compact" {
					opts.Alt = "responses/compact"
				}
				if mode == "stream" {
					result, err := e.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				request := <-captured
				if got := ojson.GetBytes(request.Request, "reasoning.effort").String(); got != effort {
					t.Fatalf("baseline effort = %q, want %q", got, effort)
				}
				if got := ojson.GetBytes(request.Request, "input.0.reasoning.effort").String(); got != "ultra" {
					t.Fatalf("configuration update effort = %q, want ultra", got)
				}
			})
		}
	}
}

package handlers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type runtimeBootstrapExecutor struct {
	failOnceStreamExecutor
	attempts atomic.Int32
}

func (*runtimeBootstrapExecutor) Identifier() string { return coreauth.CodexRuntimeProvider }
func (e *runtimeBootstrapExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.attempts.Add(1)
	chunks := make(chan coreexecutor.StreamChunk, 2)
	// Let manager bootstrap finish, but leave the HTTP SSE validator waiting for
	// a complete frame so this error reaches the handler's own bootstrap retry.
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: ")}
	chunks <- coreexecutor.StreamChunk{Err: &bridge.Error{Status: 502, Message: "ambiguous runtime disconnect"}}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestCodexRuntimeBootstrapFailureIsNeverReplayed(t *testing.T) {
	e := &runtimeBootstrapExecutor{}
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(e)
	m.SetRetryConfig(3, 0, 5)
	for _, id := range []string{"runtime-bootstrap-a", "runtime-bootstrap-b"} {
		a := &coreauth.Auth{ID: id, Provider: e.Identifier(), Status: coreauth.StatusActive}
		registry.GetGlobalRegistry().RegisterClient(id, e.Identifier(), []*registry.ModelInfo{{ID: "runtime-bootstrap-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 3}}, m)
	data, _, errs := h.ExecuteStreamWithAuthManager(context.Background(), "openai-response", "runtime-bootstrap-model", []byte(`{"model":"runtime-bootstrap-model","input":[]}`), "")
	if data == nil {
		t.Fatal("request never reached handler stream bootstrap")
	}
	if data != nil {
		for range data {
		}
	}
	if errs == nil {
		t.Fatal("missing error channel")
	}
	var failures int
	for failure := range errs {
		if failure != nil {
			failures++
			if failure.StatusCode != 502 {
				t.Fatalf("status=%d", failure.StatusCode)
			}
		}
	}
	if failures != 1 || e.attempts.Load() != 1 {
		t.Fatalf("bootstrap replayed: failures=%d attempts=%d", failures, e.attempts.Load())
	}
}

package auth

import (
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestForcedWebsocketHomeOwnershipWithSSEClient(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{Codex: config.CodexConfig{ForceWebsocket: true}, Home: config.HomeConfig{Enabled: true}})
	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: "caller-a", coreexecutor.CanonicalSessionIDMetadataKey: "thread-a"}}
	req := coreexecutor.Request{Model: "gpt-5.4"}
	ctx, prepared, release := m.prepareCodexTransport(t.Context(), []string{"codex"}, req, opts)
	if coreexecutor.DownstreamWebsocket(ctx) || !coreexecutor.WebsocketExecutionSession(ctx) {
		t.Fatal("SSE framing was changed or upstream ownership missing")
	}
	auth := &Auth{ID: "forced-home", Provider: "codex", Attributes: map[string]string{"websockets": "false"}}
	reg := executionregistry.New()
	pending, err := reg.BeginDispatch()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := reg.Install(pending, executionregistry.ScopeSpec{})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := newHomeDispatchSelection(auth, nil, "codex", scope)
	if err != nil {
		t.Fatal(err)
	}
	selection.accountedModel = "gpt-5.4"
	var closed atomic.Int32
	if errBind := selection.Bind(func() error { closed.Add(1); return nil }); errBind != nil {
		t.Fatal(errBind)
	}
	selection.Retain()
	if !m.retainHomeWebsocketSelection(ctx, prepared, "gpt-5.4", selection) {
		t.Fatal("SSE request did not retain its upstream Home selection")
	}
	id := prepared.Metadata[coreexecutor.ExecutionSessionMetadataKey].(string)
	release()
	_, next, releaseNext := m.prepareCodexTransport(t.Context(), []string{"codex"}, req, opts)
	defer releaseNext()
	if next.Metadata[coreexecutor.ExecutionSessionMetadataKey] != id || closed.Load() != 0 {
		t.Fatal("request completion closed a retained upstream")
	}
	retained, _, err := m.retainedHomeSessionSelection(ctx, next, "gpt-5.4", nil)
	if err != nil || retained != selection {
		t.Fatalf("reuse=%v err=%v", retained == selection, err)
	}
	m.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	if closed.Load() != 0 {
		t.Fatal("disabling the policy interrupted an active lease")
	}
	releaseNext()
	m.CloseExecutionSession(id)
	m.CloseExecutionSession(id)
	if closed.Load() != 1 || selection.Active() {
		t.Fatalf("release count=%d active=%v", closed.Load(), selection.Active())
	}
}

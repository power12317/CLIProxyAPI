package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestBasispointsPinsToolCredentialWithoutWebsocketPreference(t *testing.T) {
	caller := t.Name()
	_, bridge, err := basispoints.Prepare([]byte(`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"function","name":"weather"}]}`), caller, "session", &basispoints.SharedCache)
	if err != nil {
		t.Fatal(err)
	}
	bridge.BindCredential(caller, "tool-account")
	_, err = bridge.Response([]byte(`{"output":[{"id":"native_bps_pin","call_id":"call_bps_pin","type":"function_call","name":"run_officejs","arguments":"{\"code\":\"{\\\"tool\\\":\\\"weather\\\",\\\"args\\\":{}}\"}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: true}, Home: config.HomeConfig{Enabled: true}})
	req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":[{"type":"function_call_output","call_id":"call_bps_pin","output":"18C"}]}`)}
	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: caller}}
	ctx, prepared, release := m.prepareCodexTransport(t.Context(), []string{"codex"}, req, opts)
	if release != nil || coreexecutor.PreferredUpstreamWebsocket(ctx) {
		t.Fatal("native transport preference remains enabled")
	}
	if prepared.Metadata[coreexecutor.PinnedAuthMetadataKey] != "tool-account" {
		t.Fatal("tool result is not pinned to its account")
	}
	if opts.Metadata[coreexecutor.PinnedAuthMetadataKey] != nil {
		t.Fatal("caller metadata was mutated")
	}
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	if m.codexWebsocketAuthEnabled(auth) {
		t.Fatal("native WebSocket reuse remains enabled")
	}
}

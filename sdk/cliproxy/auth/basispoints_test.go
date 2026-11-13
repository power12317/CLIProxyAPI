package auth

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestBasispointsDoesNotPinToolCredential(t *testing.T) {
	caller := t.Name()
	_, bridge, err := basispoints.Prepare([]byte(`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"function","name":"weather"}]}`), caller, "session", &basispoints.SharedCache)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Response([]byte(`{"output":[{"id":"native_bps_pin","call_id":"call_bps_pin","type":"function_call","name":"run_officejs","arguments":"{\"code\":\"{\\\"tool\\\":\\\"weather\\\",\\\"args\\\":{}}\"}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: true}, Home: config.HomeConfig{Enabled: true}})
	auth := &Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"websockets": "true"}, Metadata: map[string]any{"use_basispoints": true}}
	if _, err := m.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":[{"type":"function_call_output","call_id":"call_bps_pin","output":"18C"}]}`)}
	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: caller}}
	ctx, prepared, release := m.prepareCodexTransport(t.Context(), []string{"codex"}, req, opts)
	if release != nil || coreexecutor.PreferredUpstreamWebsocket(ctx) {
		t.Fatal("native transport preference remains enabled")
	}
	if prepared.Metadata[coreexecutor.PinnedAuthMetadataKey] != nil {
		t.Fatal("Basispoints must not bind requests to an account")
	}
	if opts.Metadata[coreexecutor.PinnedAuthMetadataKey] != nil {
		t.Fatal("caller metadata was mutated")
	}
	if m.codexWebsocketAuthEnabled(auth) {
		t.Fatal("native WebSocket reuse remains enabled")
	}
}

func TestBasispointsGlobalGateControlsTransportSelection(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, account := range []string{"missing", "off", "on"} {
			t.Run(fmt.Sprintf("global=%t/account=%s", global, account), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: global}, ForceWebsocket: true}}
				m.SetConfig(cfg)
				auth := &Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"websockets": "true"}, Metadata: map[string]any{}}
				if account != "missing" {
					auth.Metadata["use_basispoints"] = account == "on"
				}
				if _, err := m.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				selected := global && account == "on"
				check := func(wantBasispoints bool) {
					t.Helper()
					if m.HasBasispointsAccounts() != wantBasispoints {
						t.Fatal("wrong effective Basispoints account selection")
					}
					if m.codexWebsocketAuthEnabled(auth) == wantBasispoints {
						t.Fatal("wrong native WebSocket account eligibility")
					}
					ctx, _, release := m.prepareCodexTransport(t.Context(), []string{"codex"}, coreexecutor.Request{Model: "test-model", Payload: []byte(`{"input":"hello"}`)}, coreexecutor.Options{})
					if release != nil {
						defer release()
					}
					if coreexecutor.PreferredUpstreamWebsocket(ctx) == wantBasispoints {
						t.Fatal("wrong native WebSocket preference")
					}
				}
				check(selected)
				cfg.Codex.Basispoints.Enabled = !global
				m.SetConfig(cfg)
				check(!global && account == "on")
			})
		}
	}
}

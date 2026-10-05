package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestPrismWebsocketSelectionUsesMappedModel(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{Codex: config.CodexConfig{Prism: config.CodexPrismConfig{Enabled: true}}}
	m.SetConfig(cfg)
	a := &Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"access_token": "test", "auth_kind": AuthKindOAuth}}
	SetOAuthModelAliasesAttribute(a, []config.OAuthModelAlias{{Name: "gpt-6.1-sol", Alias: "my-sol"}})
	for _, tc := range []struct {
		model       string
		ws, allowed bool
	}{{"my-sol", true, false}, {"gpt-6.1-sol", true, false}, {"gpt-6-astra", true, true}, {"gpt-6.1-sol", false, true}} {
		ctx := t.Context()
		if tc.ws {
			ctx = core.WithDownstreamWebsocket(ctx)
		}
		ctx = m.withPrismTransportPolicy(ctx, tc.model)
		if got := authSelectionEligibilityForRequest(ctx, core.Options{}).allows(a); got != tc.allowed {
			t.Errorf("%+v: allowed %v", tc, got)
		}
	}
	cfg.Codex.Prism.Enabled = false
	m.SetConfig(cfg)
	ctx := m.withPrismTransportPolicy(core.WithDownstreamWebsocket(t.Context()), "my-sol")
	if !authSelectionEligibilityForRequest(ctx, core.Options{}).allows(a) {
		t.Fatal("disabled Prism blocked native websocket")
	}
	a.Attributes = map[string]string{AttributeRuntimeOnly: "true"}
	cfg.Codex.Prism.Enabled = true
	if a.PrismEligible(cfg) {
		t.Fatal("runtime credentials entered browser transport")
	}
}

type prismStopFixture struct{}

func (prismStopFixture) Error() string       { return "Prism adapter unavailable" }
func (prismStopFixture) StatusCode() int     { return 502 }
func (prismStopFixture) IsRequestStop() bool { return true }
func TestPrismStopCannotBeOverriddenByRetryRule(t *testing.T) {
	a := &Auth{Provider: "codex", Metadata: map[string]any{"auth_kind": AuthKindOAuth}}
	cfg := &config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{"codex": {{Status: 502, Match: []string{"Prism"}, Action: RequestScopedActionContinue}}}}
	action, ok := matchRequestScopedErrorAction(a, prismStopFixture{}, cfg)
	if !ok || action != RequestScopedActionStop {
		t.Fatalf("replay was allowed: %s %v", action, ok)
	}
}

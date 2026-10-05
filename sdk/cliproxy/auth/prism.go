package auth

import (
	"context"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// PrismEligible applies the global switch to local Codex OAuth credentials.
func (a *Auth) PrismEligible(cfg *config.Config) bool {
	return cfg != nil && cfg.Codex.Prism.Enabled && a != nil &&
		strings.EqualFold(a.Provider, "codex") && a.AuthKind() == AuthKindOAuth &&
		!IsPluginVirtualAuth(a) && !strings.EqualFold(authAttribute(a, AttributeRuntimeOnly), "true")
}

// UsesPrismForModel scopes routing to the mapped upstream model. Models outside
// the adapter catalog retain their ordinary Codex/Basispoints path.
func (a *Auth) UsesPrismForModel(cfg *config.Config, model string) bool {
	if !a.PrismEligible(cfg) {
		return false
	}
	model = thinking.ParseSuffix(strings.TrimSpace(model)).ModelName
	return slices.Contains(registry.PrismModelIDs(), model)
}

// The local selection policy is carried through both scheduler implementations.
type prismTransportPolicyKey struct{}

func (m *Manager) withPrismTransportPolicy(ctx context.Context, model string) context.Context {
	if !core.DownstreamWebsocket(ctx) && !core.RequiredUpstreamWebsocket(ctx) {
		return ctx
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || !cfg.Codex.Prism.Enabled {
		return ctx
	}
	return context.WithValue(ctx, prismTransportPolicyKey{}, func(a *Auth) bool {
		if !a.PrismEligible(cfg) {
			return true
		}
		requested := rewriteModelForAuth(model, a)
		alias := m.resolveExecutionAliasResultForRequested(a, requested)
		if alias.UpstreamModel != "" {
			requested = alias.UpstreamModel
		}
		return !a.UsesPrismForModel(cfg, requested)
	})
}

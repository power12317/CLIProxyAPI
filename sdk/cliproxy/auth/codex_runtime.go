package auth

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const CodexRuntimeProvider = "codex-runtime"

type codexRuntimeSelectionKey struct{}

type codexRuntimeSelectionPolicy struct {
	enabled     bool
	basispoints bool
}

func (m *Manager) withCodexRuntimeSelection(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := m.runtimeConfigSnapshot()
	policy := codexRuntimeSelectionPolicy{}
	if cfg != nil {
		policy.enabled = !cfg.Home.Enabled && codexshared.RuntimeEnabled(cfg.AuthDir, cfg.Codex.Runtime.Enabled)
		policy.basispoints = cfg.Codex.Basispoints.Enabled
	}
	return context.WithValue(ctx, codexRuntimeSelectionKey{}, policy)
}

func codexRuntimeOwnsSelectedRequest(ctx context.Context, auth *Auth) bool {
	if ctx == nil || !IsCodexRuntimeOwnedAuth(auth) {
		return false
	}
	policy, _ := ctx.Value(codexRuntimeSelectionKey{}).(codexRuntimeSelectionPolicy)
	return policy.enabled && !(policy.basispoints && IsCodexUseBasispoints(auth))
}

// CodexRefreshManaged uses the global mode, independently of individual worker flags.
func CodexRefreshManaged(cfg *config.Config, auth *Auth) bool {
	return auth != nil && strings.EqualFold(auth.Provider, "codex") && cfg != nil &&
		codexshared.RuntimeEnabled(cfg.AuthDir, cfg.Codex.Runtime.Enabled)
}

func (m *Manager) codexRefreshManaged(auth *Auth) bool {
	return CodexRefreshManaged(m.runtimeConfigSnapshot(), auth)
}

// IsCodexRuntimeOwnedAuth checks the effective flag on an ordinary CPA credential.
func IsCodexRuntimeOwnedAuth(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(auth.Provider, "codex") {
		return false
	}
	state, shared := codexshared.Get(auth.Metadata)
	disabled, _ := auth.Metadata["disabled"].(bool)
	return shared && state.Enabled && !disabled
}

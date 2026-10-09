package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const CodexRuntimeProvider = "codex-runtime"

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

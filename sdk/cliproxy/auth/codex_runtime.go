package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
)

const CodexRuntimeProvider = "codex-runtime"

// IsCodexRuntimeOwnedAuth checks the effective flag on an ordinary CPA credential.
func IsCodexRuntimeOwnedAuth(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(auth.Provider, "codex") {
		return false
	}
	state, shared := codexshared.Get(auth.Metadata)
	disabled, _ := auth.Metadata["disabled"].(bool)
	return shared && state.Enabled && !disabled
}

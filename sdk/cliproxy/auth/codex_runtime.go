package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
)

const CodexRuntimeProvider = "codex-runtime"
const AttributeCodexRuntimeID = "codex_runtime_id"

// IsCodexRuntimeAuth identifies references whose credentials belong to Codex.
func IsCodexRuntimeAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), CodexRuntimeProvider)
}

func IsCodexRuntimeOwnedAuth(auth *Auth) bool {
	if IsCodexRuntimeAuth(auth) {
		return true
	}
	if auth == nil || !strings.EqualFold(auth.Provider, "codex") {
		return false
	}
	state, shared := codexshared.Get(auth.Metadata)
	return shared && state.Owner == "codex"
}

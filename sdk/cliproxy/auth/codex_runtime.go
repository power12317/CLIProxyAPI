package auth

import "strings"

const CodexRuntimeProvider = "codex-runtime"
const AttributeCodexRuntimeID = "codex_runtime_id"

// IsCodexRuntimeAuth identifies references whose credentials belong to Codex.
func IsCodexRuntimeAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), CodexRuntimeProvider)
}

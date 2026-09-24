package helps

import "strings"

// IsCodexClientUserAgent recognizes identities emitted by native Codex clients.
func IsCodexClientUserAgent(userAgent string) bool {
	for _, prefix := range []string{"codex-tui/", "codex_cli_rs/", "codex_exec/", "Codex Desktop/"} {
		if strings.HasPrefix(userAgent, prefix) {
			return true
		}
	}
	return false
}

// CodexClientVersion is the shared fallback for Codex model and infrastructure requests.
const CodexClientVersion = "0.156.1"

const CodexDefaultUserAgent = "codex-tui/" + CodexClientVersion + " (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; " + CodexClientVersion + ")"

// CodexSystemUserAgent preserves the existing credential platform profile.
func CodexSystemUserAgent(system string) string {
	platform := "Mac OS 26.5.2; arm64"
	if system == "windows" {
		platform = "Windows 10.0.19044; x86_64"
	}
	return "codex-tui/" + CodexClientVersion + " (" + platform + ") unknown (codex-tui; " + CodexClientVersion + ")"
}

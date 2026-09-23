package helps

// CodexClientVersion is the shared fallback for Codex model and infrastructure requests.
const CodexClientVersion = "0.156.0"

const CodexDefaultUserAgent = "codex-tui/" + CodexClientVersion + " (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; " + CodexClientVersion + ")"

// CodexSystemUserAgent preserves the existing credential platform profile.
func CodexSystemUserAgent(system string) string {
	platform := "Mac OS 26.5.2; arm64"
	if system == "windows" {
		platform = "Windows 10.0.19044; x86_64"
	}
	return "codex-tui/" + CodexClientVersion + " (" + platform + ") unknown (codex-tui; " + CodexClientVersion + ")"
}

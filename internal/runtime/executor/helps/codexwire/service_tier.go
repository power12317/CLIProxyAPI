// Package codexwire holds wire rules shared by Codex transports and adapters.
package codexwire

import "strings"

// ServiceTier emits a tier only for an explicitly selected accelerated mode.
// Ordinary/default/auto requests omit the optional field entirely.
func ServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "priority", "fast":
		return "priority"
	case "ultrafast":
		return "ultrafast"
	default:
		return ""
	}
}

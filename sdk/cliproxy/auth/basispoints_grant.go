package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// IsCodexUseBasispoints selects the independently authorized Office partition.
func IsCodexUseBasispoints(a *Auth) bool {
	if !IsCodexAuthRecord(a) {
		return false
	}
	if a.Attributes != nil {
		if value, ok := parseBoolAny(a.Attributes["use_basispoints"]); ok {
			return value
		}
	}
	value, _ := parseBoolAny(a.Metadata["use_basispoints"])
	return value
}

func BasispointsGrant(a *Auth) map[string]any {
	if a == nil {
		return nil
	}
	grant, _ := a.Metadata["basispoints"].(map[string]any)
	return grant
}

func BasispointsGrantStatus(a *Auth, now time.Time) string {
	grant := BasispointsGrant(a)
	if len(grant) == 0 {
		return "off"
	}
	if grant["_access_token_rejected"] == true {
		return "invalid"
	}
	if _, _, _, err := codex.ValidateBasispointsGrant(grant, now, false); err != nil {
		return "invalid"
	}
	if errorMessage, _ := grant["error"].(string); strings.TrimSpace(errorMessage) != "" {
		return "invalid"
	}
	return "valid"
}

// HasBasispointsAccounts selects full-history transport before account selection.
func (m *Manager) HasBasispointsAccounts() bool {
	if m == nil {
		return false
	}
	for _, auth := range m.List() {
		if !auth.Disabled && IsCodexUseBasispoints(auth) {
			return true
		}
	}
	return false
}

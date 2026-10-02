package auth

import (
	"strconv"
	"strings"
)

const (
	AuthKindAPIKey = "apikey"
	AuthKindOAuth  = "oauth"

	AuthSourceConfig      = "config"
	AuthSourceFile        = "file"
	AuthSourceGit         = "git"
	AuthSourceMemory      = "memory"
	AuthSourceObjectStore = "objectstore"
	AuthSourcePostgres    = "postgres"

	AttributeAPIKey               = "api_key"
	AttributeAuthKind             = "auth_kind"
	AttributeCodexAlphaSearch     = "codex_alpha_search"
	AttributeCodexDisableCloaking = "codex_disable_cloaking"
	AttributeConfigIndex          = "config_index"
	AttributePath                 = "path"
	AttributeRuntimeOnly          = "runtime_only"
	AttributeSource               = "source"
	AttributeSourceBackend        = "source_backend"
	AttributeWeight               = "weight"
	// AttributePrismBrowser selects the local Prism browser adapter for Codex OAuth.
	AttributePrismBrowser = "openai_prism_browser"
)

// UsesPrismBrowser reports whether this auth has an explicit account-level
// opt-in marker. The global Prism switch is evaluated by the executor.
func (a *Auth) UsesPrismBrowser() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") || a.AuthKind() != AuthKindOAuth {
		return false
	}
	if IsPluginVirtualAuth(a) || strings.EqualFold(strings.TrimSpace(authAttribute(a, AttributeRuntimeOnly)), "true") {
		return false
	}
	if a.Metadata != nil {
		if enabled, ok := a.Metadata[AttributePrismBrowser].(bool); ok {
			return enabled
		}
		if enabled, ok := a.Metadata[AttributePrismBrowser].(string); ok {
			parsed, errParse := strconv.ParseBool(strings.TrimSpace(enabled))
			return errParse == nil && parsed
		}
	}
	return false
}

// PrismBrowserExplicitlyDisabled reports an account-level opt-out. The global
// Prism switch remains the primary P1 control; this preserves compatibility
// with management clients that do not expose per-account settings.
func (a *Auth) PrismBrowserExplicitlyDisabled() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") || a.AuthKind() != AuthKindOAuth {
		return false
	}
	if a.Metadata == nil {
		return false
	}
	value, exists := a.Metadata[AttributePrismBrowser]
	if !exists {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return !typed
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
		return errParse == nil && !parsed
	default:
		return false
	}
}

// AuthKind returns the credential kind using explicit metadata first and legacy
// field-shape fallbacks second.
func (a *Auth) AuthKind() string {
	if a == nil {
		return ""
	}
	if kind := normalizeAuthKind(authAttribute(a, AttributeAuthKind)); kind != "" {
		return kind
	}
	if kind := normalizeAuthKind(authMetadataString(a, AttributeAuthKind)); kind != "" {
		return kind
	}
	if authAttribute(a, AttributeAPIKey) != "" {
		return AuthKindAPIKey
	}
	if authHasOAuthMetadata(a) {
		return AuthKindOAuth
	}
	return ""
}

// AuthSourceKind returns where the Auth entry came from at runtime.
func (a *Auth) AuthSourceKind() string {
	if a == nil {
		return ""
	}
	if strings.EqualFold(authAttribute(a, AttributeRuntimeOnly), "true") {
		return AuthSourceMemory
	}
	if source := normalizeAuthSourceKind(authAttribute(a, AttributeSourceBackend)); source != "" {
		return source
	}
	source := authAttribute(a, AttributeSource)
	if source != "" {
		sourceLower := strings.ToLower(source)
		if strings.HasPrefix(sourceLower, AuthSourceConfig+":") {
			return AuthSourceConfig
		}
		if normalized := normalizeAuthSourceKind(source); normalized != "" {
			return normalized
		}
		return AuthSourceFile
	}
	if authAttribute(a, AttributePath) != "" {
		return AuthSourceFile
	}
	if strings.TrimSpace(a.FileName) != "" {
		return AuthSourceFile
	}
	return ""
}

func normalizeAuthKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case AuthKindAPIKey, "api_key", "api-key":
		return AuthKindAPIKey
	case AuthKindOAuth, "oauth2":
		return AuthKindOAuth
	default:
		return ""
	}
}

func normalizeAuthSourceKind(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case AuthSourceConfig:
		return AuthSourceConfig
	case AuthSourceFile, "filesystem":
		return AuthSourceFile
	case AuthSourceGit:
		return AuthSourceGit
	case AuthSourceMemory, "runtime", "runtime_only":
		return AuthSourceMemory
	case AuthSourceObjectStore, "object-store":
		return AuthSourceObjectStore
	case AuthSourcePostgres, "postgresql", "database", "db":
		return AuthSourcePostgres
	default:
		return ""
	}
}

func authHasOAuthMetadata(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "email", "token_type", "expires_at", "expired"} {
		if authMetadataString(auth, key) != "" {
			return true
		}
	}
	if token, ok := auth.Metadata["token"].(map[string]any); ok && len(token) > 0 {
		return true
	}
	return false
}

func authAttribute(auth *Auth, key string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[key])
}

func authMetadataString(auth *Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	switch value := auth.Metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

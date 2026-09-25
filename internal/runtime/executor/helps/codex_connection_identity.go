package helps

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// CodexOwnerFingerprint distinguishes credential owners without retaining tokens.
// Token renewal for the same owner does not change this fingerprint.
func CodexOwnerFingerprint(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	parts := []string{auth.Provider, CodexOAuthAccountID(auth), CodexOAuthClientSystem(auth)}
	for _, key := range []string{"email", "chatgpt_user_id"} {
		value, _ := auth.Metadata[key].(string)
		parts = append(parts, strings.ToLower(strings.TrimSpace(value)))
	}
	if token := strings.Split(CodexOAuthAccessToken(auth), "."); len(token) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(token[1]); err == nil {
			for _, path := range []string{"sub", "https://api\\.openai\\.com/auth.chatgpt_account_id", "https://api\\.openai\\.com/auth.chatgpt_user_id"} {
				parts = append(parts, gjson.GetBytes(payload, path).String())
			}
		}
	}
	return codexIdentityDigest(parts)
}

// CodexConnectionFingerprint contains only handshake-stable settings. Per-turn
// headers and infrastructure cookies must not force a reconnect on every request.
// proxyURL is the final execution proxy resolved by the caller.
func CodexConnectionFingerprint(auth *cliproxyauth.Auth, headers http.Header, proxyURL string) string {
	stable := make(http.Header)
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "cookie", "x-codex-turn-state", "x-codex-turn-metadata", "x-client-request-id", "x-codex-window-id", "session-id", "session_id", "thread-id", "conversation_id":
			continue
		case "x-codex-routing-hint":
			if CodexAuthUsesOAuthCookieJar(auth) {
				stable[strings.ToLower(key)] = make([]string, len(values))
				for i, value := range values {
					stable[strings.ToLower(key)][i] = codexConnectionRoutingHint(value)
				}
				continue
			}
		}
		stable[strings.ToLower(key)] = values
	}
	return codexIdentityDigest([]any{CodexOwnerFingerprint(auth), stable, strings.TrimSpace(proxyURL)})
}

// Fast is a per-request body setting even on an existing websocket. Ignore only
// the tier in a standard routing hint; model and custom routing changes still
// require a new handshake, as do credential, identity, and proxy changes.
func codexConnectionRoutingHint(value string) string {
	model, tier, found := strings.Cut(value, ";tier=")
	if found && strings.HasPrefix(model, "model=") && len(model) > len("model=") &&
		!strings.Contains(model, ";") && tier != "" && !strings.Contains(tier, ";") {
		return model
	}
	return value
}

func codexIdentityDigest(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

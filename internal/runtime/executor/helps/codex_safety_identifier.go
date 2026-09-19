package helps

import (
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyCodexSafetyIdentifier binds every OAuth JSON request to the selected
// credential, independently of the official-client marker and payload overrides.
// Re-read the current token so imported auth files and rotated tokens do not use
// a stale cached identity. Non-JSON payloads (such as multipart images) are unchanged.
func ApplyCodexSafetyIdentifier(body []byte, auth *cliproxyauth.Auth) []byte {
	if auth == nil || auth.AuthKind() != cliproxyauth.AuthKindOAuth || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return body
	}
	token, _ := auth.Metadata["access_token"].(string)
	userID := codexauth.ChatGPTUserIDFromAccessToken(token)
	if userID == "" {
		userID, _ = auth.Metadata["chatgpt_user_id"].(string)
	}
	var updated []byte
	var errUpdate error
	if userID == "" {
		// An opaque or malformed legacy token cannot establish a user identity.
		// Never forward a caller-supplied or cached identity for such a token.
		updated, errUpdate = sjson.DeleteBytes(body, "safety_identifier")
	} else {
		updated, errUpdate = sjson.SetBytes(body, "safety_identifier", userID)
	}
	if errUpdate != nil {
		return body
	}
	return updated
}

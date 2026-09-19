package helps

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestApplyCodexSafetyIdentifierAlwaysUsesOAuthUserID(t *testing.T) {
	token := testAccessTokenWithChatGPTUserID(t, "chatgpt-user-1")
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"auth_kind":    cliproxyauth.AuthKindOAuth,
			"access_token": token,
		},
	}

	for _, body := range []string{
		`{"safety_identifier":"caller-value","input":[]}`,
		`{"input":[]}`,
	} {
		updated := ApplyCodexSafetyIdentifier([]byte(body), auth)
		if got := gjson.GetBytes(updated, "safety_identifier").String(); got != "chatgpt-user-1" {
			t.Fatalf("safety_identifier = %q, want chatgpt-user-1; body=%s", got, updated)
		}
	}
}

func TestApplyCodexSafetyIdentifierUsesPersistedIdentityForLegacyToken(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"auth_kind":       cliproxyauth.AuthKindOAuth,
			"access_token":    "opaque-token",
			"chatgpt_user_id": "persisted-user",
		},
	}
	updated := ApplyCodexSafetyIdentifier([]byte(`{"safety_identifier":"caller-value"}`), auth)
	if got := gjson.GetBytes(updated, "safety_identifier").String(); got != "persisted-user" {
		t.Fatalf("safety_identifier = %q, want persisted-user", got)
	}
}

func testAccessTokenWithChatGPTUserID(t *testing.T, userID string) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_user_id": userID},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

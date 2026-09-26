package helps

import (
	"net/http"
	"reflect"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexConnectionFingerprintIgnoresHandshakeOptions(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": "account"}}
		if apiKey {
			auth.Attributes = map[string]string{"api_key": "token"}
		}
		base := http.Header{"Authorization": {"Bearer token"}}
		want := CodexConnectionFingerprint(auth, base, "direct")
		for _, key := range []string{"OpenAI-Beta", "User-Agent", "Originator", "Version", "X-Codex-Beta-Features", "X-Codex-Routing-Hint", "X-Custom-Header", "Session-Id", "Thread-Id", "X-Client-Request-Id", "Cookie", "X-Codex-Turn-State"} {
			headers := base.Clone()
			for _, value := range []string{"old", "new", "model=gpt-6-astra;tier=priority", "model=gpt-6-luna", "operator-route"} {
				headers.Set(key, value)
				original := headers.Clone()
				if CodexConnectionFingerprint(auth, headers, "direct") != want {
					t.Fatalf("api_key=%t: %s change forces a reconnect", apiKey, key)
				}
				if !reflect.DeepEqual(headers, original) {
					t.Fatal("fingerprint mutated handshake headers")
				}
			}
			headers.Del(key)
			if CodexConnectionFingerprint(auth, headers, "direct") != want {
				t.Fatalf("removing %s forces a reconnect", key)
			}
		}
	}
}

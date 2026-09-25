package helps

import (
	"net/http"
	"reflect"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexConnectionFingerprintRoutingTierReuse(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": "account"}}
	fingerprint := func(hint string) string {
		headers := http.Header{"X-Codex-Routing-Hint": {hint}}
		original := headers.Clone()
		result := CodexConnectionFingerprint(auth, headers, "")
		if !reflect.DeepEqual(headers, original) {
			t.Fatal("fingerprint changed the handshake routing hint")
		}
		return result
	}
	standard := fingerprint("model=gpt-5.5")
	for _, hint := range []string{"model=gpt-5.5;tier=priority", "model=gpt-5.5;tier=ultrafast"} {
		if fingerprint(hint) != standard {
			t.Fatalf("tier-only change %q forces a reconnect", hint)
		}
	}
	for _, hint := range []string{"model=gpt-6-astra;tier=priority", "model=gpt-5.5;route=pool;tier=priority", "model=gpt-5.5;tier=priority;route=pool", "operator-route"} {
		if fingerprint(hint) == standard {
			t.Fatalf("model/custom routing change %q reuses the connection", hint)
		}
	}
	auth.Attributes = map[string]string{"api_key": "sk-test"}
	if fingerprint("model=gpt-5.5;tier=priority") == fingerprint("model=gpt-5.5") {
		t.Fatal("API-key operator hints lost their connection isolation")
	}
}

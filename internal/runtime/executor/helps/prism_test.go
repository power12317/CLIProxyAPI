package helps

import (
	"net/http"
	"testing"

	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPrismIdentityScopesCallerAccountAndThread(t *testing.T) {
	a := &auth.Auth{ID: "account", Metadata: map[string]any{"account_id": "uuid-account"}}
	req := core.Request{Payload: []byte(`{"input":"hello","client_metadata":{"thread_id":"thread-a"}}`)}
	opts := core.Options{Headers: http.Header{"Session-Id": []string{"process"}, "X-Prism-Session-Id": []string{"forged"}}, Metadata: map[string]any{core.CallerScopeMetadataKey: "caller-a"}}
	account, caller, conversation := PrismIdentity(a, req, opts)
	if len(account) != 64 || len(caller) != 64 || len(conversation) != 64 {
		t.Fatal("missing hashed identity")
	}
	opts.Headers.Set("Session-Id", "another-process")
	_, _, same := PrismIdentity(a, req, opts)
	if same != conversation {
		t.Fatal("thread did not take precedence")
	}
	req.Payload = []byte(`{"client_metadata":{"thread_id":"thread-b"}}`)
	_, _, different := PrismIdentity(a, req, opts)
	if different == conversation {
		t.Fatal("threads share a project")
	}
	opts.Metadata[core.CallerScopeMetadataKey] = "caller-b"
	_, otherCaller, _ := PrismIdentity(a, req, opts)
	if otherCaller == caller {
		t.Fatal("callers share tool state")
	}
	opts.Headers = nil
	req.Payload = []byte(`{"input":"hello"}`)
	_, _, empty := PrismIdentity(a, req, opts)
	if empty != "" {
		t.Fatal("anonymous input reused a project")
	}
	opts.Metadata = nil
	_, emptyCaller, _ := PrismIdentity(a, req, opts)
	if emptyCaller != "" {
		t.Fatal("unverified caller accepted")
	}
}

package helps

import (
	"net/http"
	"testing"
)

func TestCodexOAuthRoutingHintUsesFinalBody(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"model":"final-model"}`, "model=final-model"},
		{`{"model":"final-model","service_tier":null}`, "model=final-model"},
		{`{"model":"final-model","service_tier":""}`, "model=final-model"},
		{`{"model":"final-model","service_tier":123}`, "model=final-model"},
		{`{"model":"final-model","service_tier":"default"}`, "model=final-model;tier=default"},
		{`{"model":"final-model","service_tier":"priority"}`, "model=final-model;tier=priority"},
		{`{"model":"final-model","service_tier":"ultrafast"}`, "model=final-model;tier=ultrafast"},
	} {
		headers := http.Header{"X-Codex-Routing-Hint": {"model=stale;tier=stale"}}
		ApplyCodexOAuthRoutingHint(headers, []byte(tc.body))
		if got := headers.Get("X-Codex-Routing-Hint"); got != tc.want {
			t.Errorf("body %s: got %q, want %q", tc.body, got, tc.want)
		}
	}
}

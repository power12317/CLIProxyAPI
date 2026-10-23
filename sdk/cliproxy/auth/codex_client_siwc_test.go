package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCodexClientSIWCPreferenceSelectsRequestPartition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		allowed bool
	}{
		{"missing", nil, true}, {"enabled", true, true}, {"disabled", false, false}, {"legacy string", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: "codex", Metadata: map[string]any{"use_siwc": true, "access_token": "outer", "siwc": map[string]any{"access_token": "siwc"}}}
			if tc.value != nil {
				auth.Metadata["allow_codex_client_siwc"] = tc.value
			}
			headers := http.Header{"User-Agent": {"codex-tui/0.163.0"}}
			if got := IsCodexUseSIWCForRequest(context.Background(), auth, headers); got != tc.allowed {
				t.Fatalf("Codex SIWC=%v, want %v", got, tc.allowed)
			}
			ginCtx := &gin.Context{Request: &http.Request{Header: headers}}
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			want := "outer"
			if tc.allowed {
				want = "siwc"
			}
			if got := authAccessTokenForRequest(ctx, auth); got != want {
				t.Fatalf("request token=%q, want %q", got, want)
			}
			if !IsCodexUseSIWCForRequest(context.Background(), auth, http.Header{"User-Agent": {"curl/8"}}) {
				t.Fatal("non-Codex client lost SIWC")
			}
			auth.Metadata["use_siwc"] = false
			if IsCodexUseSIWCForRequest(ctx, auth) {
				t.Fatal("client allowance enabled an inactive SIWC partition")
			}
		})
	}
}

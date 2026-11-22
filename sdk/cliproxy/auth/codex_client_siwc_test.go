package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSIWCRoutingIsIndependentOfClientAndLegacySwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, legacy := range []any{nil, false, true, "false"} {
			for _, ua := range []string{"codex-tui/0.171.0", "Codex Desktop/26.1111.71704", "curl/8"} {
				t.Run(ua, func(t *testing.T) {
					auth := &Auth{Provider: "codex", Metadata: map[string]any{
						"use_siwc": enabled, "access_token": "outer",
						"siwc": map[string]any{"access_token": "siwc"},
					}}
					if legacy != nil {
						auth.Metadata["allow_codex_client_siwc"] = legacy
					}
					headers := http.Header{"User-Agent": {ua}}
					ginCtx := &gin.Context{Request: &http.Request{Header: headers}}
					ctx := context.WithValue(context.Background(), "gin", ginCtx)
					if got := IsCodexUseSIWCForRequest(ctx, auth, headers); got != enabled {
						t.Fatalf("SIWC=%v, want %v, legacy=%v", got, enabled, legacy)
					}
					want := "outer"
					if enabled {
						want = "siwc"
					}
					if got := authAccessTokenForRequest(ctx, auth); got != want {
						t.Fatalf("request token=%q, want %q", got, want)
					}
					if IsCodexUseSIWCForRequest(WithCodexImageRequest(ctx), auth, headers) {
						t.Fatal("image request used SIWC")
					}
				})
			}
		}
	}
}

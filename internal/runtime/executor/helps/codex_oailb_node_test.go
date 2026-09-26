package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestOaiLBNodeExtraction(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"chat.gateway.unified-96.api.openai.com", "unified-96"},
		{"CHAT.GATEWAY.UNIFIED-96.API.OPENAI.COM.", "unified-96"},
		{"chat.gateway.unified-96.api.openai.com.evil.test", ""},
		{"chat.gateway.a.b.api.openai.com", ""},
		{"chat.gateway.-bad.api.openai.com", ""},
		{"chat.gateway.bad\nnode.api.openai.com", ""},
		{"chat.gateway." + strings.Repeat("a", 64) + ".api.openai.com", ""},
	} {
		// The node of a live websocket remains observable after JWT expiration.
		if got := CodexOaiLBNode(oaiLBTestJWT(time.Unix(1, 0), tc.host)); got != tc.want {
			t.Errorf("host %q got %q want %q", tc.host, got, tc.want)
		}
	}
	if CodexOaiLBNode("invalid.jwt") != "" {
		t.Fatal("invalid cookie produced a node")
	}
}

func TestOaiLBNodeResponseFirstAndActualRequestFallback(t *testing.T) {
	requestJWT := oaiLBTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-96.api.openai.com")
	responseJWT := oaiLBTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-42.api.openai.com")
	headers := http.Header{"Cookie": {"__cf_bm=local; __oailb=" + requestJWT}}
	for _, tc := range []struct{ cookie, want string }{
		{"", "unified-96"},
		{"__cflb=opaque; Path=/", "unified-96"},
		{"__oailb=" + responseJWT + "; Path=/", "unified-42"},
		{"__oailb=invalid; Path=/", ""},
		{"__oailb=; Max-Age=0; Path=/", ""},
	} {
		response := &http.Response{Header: make(http.Header)}
		if tc.cookie != "" {
			response.Header.Add("Set-Cookie", tc.cookie)
		}
		if got := CodexOaiLBNodeForExchange(headers, response); got != tc.want {
			t.Errorf("cookie %q got %q want %q", tc.cookie, got, tc.want)
		}
	}
}

func TestOaiLBNodeHTTPUsageWithoutBorrowing(t *testing.T) {
	for _, scenario := range []string{"request", "response", "network-failure"} {
		t.Run(scenario, func(t *testing.T) {
			credential := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
			defer InvalidateCodexCookieJar(credential.ID)
			u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
			requestJWT := oaiLBTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-96.api.openai.com")
			responseJWT := oaiLBTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-42.api.openai.com")
			CodexCookieJarForAuth(credential).SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: requestJWT, Path: "/"}})
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := context.WithValue(t.Context(), "gin", ginCtx)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", oaiLBTestTransport(func(r *http.Request) (*http.Response, error) {
				if scenario == "network-failure" {
					return nil, errors.New("simulated failure")
				}
				h := make(http.Header)
				if scenario == "response" {
					h.Add("Set-Cookie", "__oailb="+responseJWT+"; Path=/")
				}
				return &http.Response{StatusCode: 400, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
			}))
			reporter := NewUsageReporter(ctx, "codex", "model", credential)
			client := reporter.TrackHTTPClientRoundTripOnly(NewUtlsHTTPClient(ctx, &config.Config{}, credential, 0))
			req, _ := http.NewRequestWithContext(ctx, "POST", u.String(), nil)
			resp, err := client.Do(req)
			if scenario != "network-failure" && err != nil {
				t.Fatal(err)
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			want := "unified-96"
			if scenario == "response" {
				want = "unified-42"
			}
			record := reporter.buildRecord(usage.Detail{}, true)
			if record.OaiLBNode != want || logging.CodexOaiLBNode(ginCtx) != want {
				t.Fatalf("wrong node: record=%q log=%q", record.OaiLBNode, logging.CodexOaiLBNode(ginCtx))
			}
			// Later turn-state updates must not erase the transport observation.
			logging.SetCodexTurnStateLogFields(ginCtx, logging.CodexTurnStateLogFields{TurnID: "new-turn"})
			if logging.CodexOaiLBNode(ginCtx) != want {
				t.Fatal("node erased by turn metadata")
			}
		})
	}
}

func TestCFLBHasNoBorrowExpiry(t *testing.T) {
	start := time.Unix(1800000000, 0)
	now := start
	e := &oaiLBBorrowEntry{}
	value := CodexRoutingCookies{OaiLB: oaiLBTestJWT(start.Add(time.Hour), "host"), CFLB: "opaque-cflb"}
	fetched := true
	fetch := func(context.Context) (CodexRoutingCookies, bool) { return value, fetched }
	get := func() CodexRoutingCookies { return e.get(t.Context(), func() time.Time { return now }, fetch) }
	if got := get(); got != value {
		t.Fatal("initial pair was not cached")
	}
	now = start.Add(2 * time.Hour)
	value = CodexRoutingCookies{}
	fetched = false
	if got := get(); got.OaiLB != "" || got.CFLB != "opaque-cflb" {
		t.Fatalf("CFLB incorrectly expired with JWT: %#v", got)
	}
	now = now.Add(time.Minute)
	fetched = true
	if got := get(); !got.empty() {
		t.Fatal("successful snapshot did not remove missing CFLB")
	}
}

func TestCFLBOnlyBorrowResponse(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"available":true,"value":"","cflb":"opaque"}`)
	}))
	defer source.Close()
	cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: source.URL, SourceManagementKey: "key", SourceAuthID: "auth"}}}
	a := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
	h := http.Header{"Cookie": {"__oailb=local; __cflb=old; __cf_bm=mine"}}
	got := PrepareCodexOaiLBBorrow(t.Context(), cfg, a, "https://chatgpt.com/backend-api/codex/responses", h)
	if got.Get("Cookie") != "__oailb=local; __cf_bm=mine; __cflb=opaque" {
		t.Fatal(got.Get("Cookie"))
	}
}

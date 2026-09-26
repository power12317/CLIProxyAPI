package helps

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		if got := CodexOaiLBNode(oaiLBNodeTestJWT(time.Unix(1, 0), tc.host)); got != tc.want {
			t.Errorf("host %q got %q want %q", tc.host, got, tc.want)
		}
	}
	if CodexOaiLBNode("invalid.jwt") != "" {
		t.Fatal("invalid cookie produced a node")
	}
}

func TestOaiLBNodeResponseFirstAndActualRequestFallback(t *testing.T) {
	requestJWT := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-96.api.openai.com")
	responseJWT := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-42.api.openai.com")
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

func TestOaiLBNodeHTTPUsageFromLocalCookieJar(t *testing.T) {
	for _, scenario := range []string{"request", "response", "network-failure"} {
		t.Run(scenario, func(t *testing.T) {
			credential := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
			defer InvalidateCodexCookieJar(credential.ID)
			u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
			requestJWT := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-96.api.openai.com")
			responseJWT := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-42.api.openai.com")
			jar := CodexCookieJarForAuth(credential)
			jar.SetCookies(u, []*http.Cookie{
				{Name: "__oailb", Value: requestJWT, Path: "/"},
				{Name: "__cflb", Value: "local-cflb", Path: "/"},
				{Name: "__cf_bm", Value: "local-cf-bm", Path: "/"},
			})
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := context.WithValue(t.Context(), "gin", ginCtx)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", oaiLBNodeTestTransport(func(r *http.Request) (*http.Response, error) {
				for name, want := range map[string]string{"__oailb": requestJWT, "__cflb": "local-cflb", "__cf_bm": "local-cf-bm"} {
					cookie, errCookie := r.Cookie(name)
					if errCookie != nil || cookie.Value != want {
						t.Fatalf("node observation changed outgoing cookie %s", name)
					}
				}
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
			if scenario == "network-failure" && err == nil {
				t.Fatal("network failure was hidden")
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
			jarHeaders := make(http.Header)
			jarRequest := &http.Request{Header: jarHeaders}
			for _, cookie := range jar.Cookies(u) {
				jarRequest.AddCookie(cookie)
			}
			if got := CodexOaiLBNodeForExchange(jarHeaders, nil); got != want {
				t.Fatalf("local cookie jar node = %q, want %q", got, want)
			}
			// Later turn-state updates must not erase the transport observation.
			logging.SetCodexTurnStateLogFields(ginCtx, logging.CodexTurnStateLogFields{TurnID: "new-turn"})
			if logging.CodexOaiLBNode(ginCtx) != want {
				t.Fatal("node erased by turn metadata")
			}
		})
	}
}

func oaiLBNodeTestJWT(exp time.Time, host string) string {
	body, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "host": host})
	return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

type oaiLBNodeTestTransport func(*http.Request) (*http.Response, error)

func (f oaiLBNodeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

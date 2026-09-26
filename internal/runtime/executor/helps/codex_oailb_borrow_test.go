package helps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func oaiLBTestJWT(exp time.Time, label string) string {
	body, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "host": label})
	return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func TestOaiLBBorrowRefreshGraceAndHardExpiry(t *testing.T) {
	start := time.Unix(1800000000, 0)
	now := start
	value := oaiLBTestJWT(start.Add(65*time.Minute), "first")
	e := &oaiLBBorrowEntry{}
	calls := 0
	fetch := func(context.Context) string { calls++; return value }
	get := func() string { return e.get(t.Context(), func() time.Time { return now }, fetch) }
	first := get()
	if first == "" || calls != 1 {
		t.Fatal("initial acquisition failed")
	}
	now = start.Add(59 * time.Minute)
	if get() != first || calls != 1 {
		t.Fatal("refreshed before exp-300")
	}
	now = start.Add(60 * time.Minute)
	value = ""
	if get() != first || calls != 2 {
		t.Fatal("failed refresh lost the JWT grace period")
	}
	now = now.Add(10 * time.Second)
	if get() != first || calls != 2 {
		t.Fatal("retry gate did not suppress repeated failure")
	}
	now = start.Add(65 * time.Minute)
	if get() != "" || calls != 3 {
		t.Fatal("hard-expired JWT did not fall back")
	}
	now = now.Add(30 * time.Second)
	value = oaiLBTestJWT(now.Add(time.Hour), "replacement")
	if get() != value || calls != 4 {
		t.Fatal("did not recover after source returned")
	}
	value = oaiLBTestJWT(now.Add(2*time.Hour), "not-yet")
	if get() == value || calls != 4 {
		t.Fatal("replaced a still-fresh lease")
	}
}

func TestOaiLBBorrowConcurrentAcquisition(t *testing.T) {
	e := &oaiLBBorrowEntry{}
	value := oaiLBTestJWT(time.Now().Add(time.Hour), "shared")
	start, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) string {
		if calls.Add(1) == 1 {
			close(start)
		}
		<-release
		return value
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.get(t.Context(), time.Now, fetch) != value {
				t.Error("wrong shared value")
			}
		}()
	}
	<-start
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate source requests", calls.Load())
	}
}

type oaiLBTestTransport func(*http.Request) (*http.Response, error)

func (f oaiLBTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOaiLBHTTPOverlayAndLocalJarIsolation(t *testing.T) {
	borrowed := oaiLBTestJWT(time.Now().Add(time.Hour), "borrowed")
	var calls atomic.Int32
	donor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			AuthID string `json:"auth_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/prefix/v0/management/codex/oailb/borrow" || r.Header.Get("Authorization") != "Bearer password" || body.AuthID != "selected-auth" {
			t.Error("incorrect donor request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"available": true, "value": borrowed})
	}))
	defer donor.Close()
	cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: donor.URL + "/prefix", SourceAuthID: "selected-auth", SourceManagementKey: "password"}}}
	credential := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "local-token"}}
	defer InvalidateCodexCookieJar(credential.ID)
	u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	jar := CodexCookieJarForAuth(credential)
	jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: "self", Path: "/"}, {Name: "__cf_bm", Value: "cf", Path: "/"}})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", oaiLBTestTransport(func(r *http.Request) (*http.Response, error) {
		seen := map[string][]string{}
		for _, c := range r.Cookies() {
			seen[c.Name] = append(seen[c.Name], c.Value)
		}
		if r.URL.Path == codexChatGPTResponses {
			if len(seen["__oailb"]) != 1 || seen["__oailb"][0] != borrowed || len(seen["__cf_bm"]) != 1 {
				t.Errorf("wrong merged cookies: %#v", seen)
			}
		} else if seen["__oailb"][0] != "self-new" {
			t.Error("usage unexpectedly borrowed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"__oailb=self-new; Path=/", "__cf_bm=cf-new; Path=/"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	}))
	client := NewUtlsHTTPClient(ctx, cfg, credential, 0)
	for _, path := range []string{codexChatGPTResponses, codexChatGPTResponses, codexChatGPTUsagePath} {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com"+path, nil)
		if path == codexChatGPTResponses {
			req.AddCookie(&http.Cookie{Name: "__oailb", Value: "explicit"})
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if calls.Load() != 1 {
		t.Fatal("fresh lease was fetched repeatedly")
	}
	if jar.Cookies(u)[0].Value != "self-new" {
		t.Fatal("borrowed value polluted local jar")
	}
	wsHeaders := PrepareCodexOaiLBBorrow(ctx, cfg, credential, "wss://chatgpt.com/backend-api/codex/responses", nil)
	if !strings.Contains(wsHeaders.Get("Cookie"), "__oailb="+borrowed) || !strings.Contains(wsHeaders.Get("Cookie"), "__cf_bm=cf-new") {
		t.Fatal("websocket cookie composition failed")
	}
	if h := PrepareCodexOaiLBBorrow(ctx, cfg, credential, "https://thirdparty.example/responses", nil); len(h) != 0 {
		t.Fatal("borrowed outside ChatGPT")
	}
	if h := PrepareCodexOaiLBBorrow(WithoutCodexOaiLBBorrow(ctx), cfg, credential, u.String(), nil); len(h) != 0 {
		t.Fatal("local probe borrowed")
	}
}

func TestOaiLBDonorRefreshesUsageWithoutTicketOrBackgroundTask(t *testing.T) {
	credential := &auth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
	defer InvalidateCodexCookieJar(credential.ID)
	value := oaiLBTestJWT(time.Now().Add(time.Hour), "usage")
	var calls atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", oaiLBTestTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != codexChatGPTUsagePath {
			t.Fatal("unexpected probe", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"__oailb=" + value + "; Path=/; Max-Age=3600; Secure"}}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))
	for i := 0; i < 2; i++ {
		got, err := BorrowCodexOaiLB(ctx, &config.Config{}, credential)
		if err != nil || got != value {
			t.Fatal("usage acquisition failed", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unnecessary refresh", calls.Load())
	}
}

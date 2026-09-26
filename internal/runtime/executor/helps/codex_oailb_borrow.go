package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const oaiLBRefreshBefore = 5 * time.Minute
const oaiLBRetryInterval = 30 * time.Second

// CodexOaiLBExpiry reads only the routing cookie's JWT expiry. It neither changes
// the signed value nor treats its claims as account authorization or a URL.
func CodexOaiLBExpiry(value string) (time.Time, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(value) > 32768 {
		return time.Time{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func validOaiLB(value string, now time.Time) bool {
	exp, ok := CodexOaiLBExpiry(value)
	return ok && now.Before(exp) && (&http.Cookie{Name: "__oailb", Value: value}).Valid() == nil
}

// The cached string is the only cookie state. Refresh scheduling is independent
// of its lifetime, which is always decoded from exp, including after failures.
type oaiLBBorrowEntry struct {
	mu          sync.Mutex
	value       string
	nextAttempt time.Time
	inflight    chan struct{}
}

var oaiLBBorrowEntries = cache.NewBoundedLRU[string, *oaiLBBorrowEntry](32, nil)

func InvalidateCodexOaiLBBorrow(cfg *config.Config) {
	if cfg != nil && cfg.CodexHeaderDefaults.OaiLBBorrow != nil {
		oaiLBBorrowEntries.Delete(codexIdentityDigest([]any{cfg.AuthDir, cfg.CodexHeaderDefaults.OaiLBBorrow}))
	}
}

func (e *oaiLBBorrowEntry) get(ctx context.Context, now func() time.Time, fetch func(context.Context) string) string {
	for {
		e.mu.Lock()
		t := now()
		exp, ok := CodexOaiLBExpiry(e.value)
		usable := ok && validOaiLB(e.value, t)
		if (usable && t.Before(exp.Add(-oaiLBRefreshBefore))) || t.Before(e.nextAttempt) {
			value := e.value
			e.mu.Unlock()
			if usable {
				return value
			}
			return ""
		}
		if done := e.inflight; done != nil {
			value := e.value
			e.mu.Unlock()
			if usable {
				return value
			}
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ""
			}
		}
		e.inflight = make(chan struct{})
		done := e.inflight
		e.mu.Unlock()
		value := fetch(ctx)
		e.mu.Lock()
		t = now()
		if validOaiLB(value, t) {
			e.value = value
		}
		e.nextAttempt = t.Add(oaiLBRetryInterval)
		value = e.value
		e.inflight = nil
		close(done)
		e.mu.Unlock()
		if validOaiLB(value, t) {
			return value
		}
		return ""
	}
}

func codexOaiLBBorrowValue(ctx context.Context, cfg *config.Config) string {
	if cfg == nil || cfg.CodexHeaderDefaults.OaiLBBorrow == nil {
		return ""
	}
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	key := codexIdentityDigest([]any{cfg.AuthDir, c})
	entry := oaiLBBorrowEntries.GetOrAdd(key, func() *oaiLBBorrowEntry { return &oaiLBBorrowEntry{} })
	value := entry.get(ctx, time.Now, func(ctx context.Context) string { return fetchCodexOaiLB(ctx, cfg) })
	return value
}

func fetchCodexOaiLB(ctx context.Context, cfg *config.Config) string {
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	if c.Validate() != nil {
		return ""
	}
	key, err := config.CodexOaiLBManagementKey(cfg)
	if err != nil {
		return ""
	}
	base := strings.TrimRight(c.SourceURL, "/")
	base = strings.TrimSuffix(base, "/v0/management")
	body, _ := json.Marshal(map[string]string{"auth_id": c.SourceAuthID})
	// This deadline only bounds cookie acquisition, never an established model stream.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v0/management/codex/oailb/borrow", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var result struct {
		Available bool   `json:"available"`
		Value     string `json:"value"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result) != nil || !result.Available {
		return ""
	}
	return result.Value
}

type noOaiLBBorrowKey struct{}

// WithoutCodexOaiLBBorrow keeps local ticket harvesting on the local jar.
func WithoutCodexOaiLBBorrow(ctx context.Context) context.Context {
	return context.WithValue(ctx, noOaiLBBorrowKey{}, true)
}

func oaiLBResponsesURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "wss") && strings.EqualFold(u.Hostname(), "chatgpt.com") && u.Path == codexChatGPTResponses
}

func replaceOaiLBCookie(headers http.Header, value string) {
	req := &http.Request{Header: headers}
	cookies := req.Cookies()
	headers.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != "__oailb" {
			req.AddCookie(cookie)
		}
	}
	req.AddCookie(&http.Cookie{Name: "__oailb", Value: value})
}

// PrepareCodexOaiLBBorrow preserves every local cookie except the borrowed name.
// Websocket callers use this only while dialing, never when reusing a connection.
func PrepareCodexOaiLBBorrow(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, rawURL string, headers http.Header) http.Header {
	if ctx == nil {
		ctx = context.Background()
	}
	if !CodexAuthUsesOAuthCookieJar(auth) || !oaiLBResponsesURL(rawURL) || ctx.Value(noOaiLBBorrowKey{}) != nil {
		return headers
	}
	value := codexOaiLBBorrowValue(ctx, cfg)
	if value == "" {
		return headers
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	if strings.HasPrefix(rawURL, "wss:") {
		headers = CodexWebsocketCookieHeaders(CodexCookieJarForAuth(auth), rawURL, headers)
	}
	replaceOaiLBCookie(headers, value)
	return headers
}

type oaiLBBorrowTransport struct {
	base http.RoundTripper
	cfg  *config.Config
	auth *cliproxyauth.Auth
}

func (t *oaiLBBorrowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	headers := PrepareCodexOaiLBBorrow(req.Context(), t.cfg, t.auth, req.URL.String(), req.Header)
	clone := req.Clone(req.Context())
	clone.Header = headers
	return t.base.RoundTrip(clone)
}

var oaiLBDonorRefresh = cache.NewBoundedLRU[string, *oaiLBBorrowEntry](128, nil)

// BorrowCodexOaiLB consults one local jar and refreshes usage on demand only.
// No consumer registry, cookie timestamp store, or background task is created.
func BorrowCodexOaiLB(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil || auth.Disabled || !CodexAuthUsesOAuthCookieJar(auth) {
		return "", errors.New("source credential unavailable")
	}
	read := func() string {
		u, _ := url.Parse("https://chatgpt.com" + codexChatGPTResponses)
		for _, c := range CodexCookieJarForAuth(auth).Cookies(u) {
			if c.Name == "__oailb" && validOaiLB(c.Value, time.Now()) {
				return c.Value
			}
		}
		return ""
	}
	value := read()
	if exp, ok := CodexOaiLBExpiry(value); ok && time.Now().Before(exp.Add(-oaiLBRefreshBefore)) {
		return value, nil
	}
	// Reuse only the synchronization/retry gate, not the entry's cached cookie.
	gate := oaiLBDonorRefresh.GetOrAdd(codexIdentityDigest([]string{auth.ID, CodexOwnerFingerprint(auth)}), func() *oaiLBBorrowEntry { return &oaiLBBorrowEntry{} })
	gate.mu.Lock()
	if done := gate.inflight; done != nil {
		gate.mu.Unlock()
		select {
		case <-done:
			return read(), nil
		case <-ctx.Done():
			return value, ctx.Err()
		}
	}
	if time.Now().Before(gate.nextAttempt) {
		gate.mu.Unlock()
		return value, nil
	}
	gate.inflight = make(chan struct{})
	done := gate.inflight
	gate.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	err := warmupCodexEndpoint(ctx, cfg, auth, codexChatGPTUsagePath)
	gate.mu.Lock()
	gate.nextAttempt = time.Now().Add(oaiLBRetryInterval)
	gate.inflight = nil
	close(done)
	gate.mu.Unlock()
	if fresh := read(); fresh != "" {
		return fresh, nil
	}
	return "", err
}

package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexChatGPTHost       = "chatgpt.com"
	codexChatGPTUsagePath  = "/backend-api/wham/usage"
	codexChatGPTAppsPath   = "/backend-api/ps/apps/batch"
	codexChatGPTResponses  = "/backend-api/codex/responses"
	codexCookieRefreshTime = 16 * time.Minute
)

var codexAllowedCookieNames = []string{
	"__cf_bm",
	"__cflb",
	"__cfruid",
	"__cfseq",
	"__cfwaitingroom",
	"_cfuvid",
	"cf_clearance",
	"cf_ob_info",
	"cf_use_ob",
}

type codexCookieJar struct {
	inner         http.CookieJar
	preserveOaiLB bool
	owner         string
}

func (j *codexCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if j == nil || !isAllowedCodexCookieURL(u) {
		return nil
	}
	return j.inner.Cookies(u)
}

func (j *codexCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j == nil || !isAllowedCodexCookieURL(u) {
		return
	}
	filtered := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || !isAllowedCodexCookieName(cookie.Name, j.preserveOaiLB) {
			continue
		}
		filtered = append(filtered, cookie)
	}
	if len(filtered) > 0 {
		j.inner.SetCookies(u, filtered)
	}
}

var codexCookieJars sync.Map // map[string]http.CookieJar, keyed by auth.ID

// CodexAuthUsesOAuthCookieJar reports whether an auth can use the ChatGPT OAuth jar.
func CodexAuthUsesOAuthCookieJar(auth *cliproxyauth.Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		return false
	}
	if auth.Metadata == nil {
		return false
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(accessToken) != ""
}

// CodexCookieJarForAuth returns the stable in-memory Cookie jar for one OAuth credential.
func CodexCookieJarForAuth(auth *cliproxyauth.Auth) http.CookieJar {
	if !CodexAuthUsesOAuthCookieJar(auth) || strings.TrimSpace(auth.ID) == "" {
		return nil
	}
	preserveOaiLB := true
	if value, ok := auth.Metadata["codex_cookie_preserve_oailb"].(bool); ok {
		preserveOaiLB = value
	}
	owner := CodexOwnerFingerprint(auth)
	for {
		existing, loaded := codexCookieJars.Load(auth.ID)
		if loaded {
			jar := existing.(*codexCookieJar)
			if jar.owner == owner && jar.preserveOaiLB == preserveOaiLB {
				return jar
			}
		}
		inner, errNewJar := cookiejar.New(nil)
		if errNewJar != nil {
			log.WithError(errNewJar).Warn("failed to create Codex cookie jar")
			return nil
		}
		jar := &codexCookieJar{inner: inner, preserveOaiLB: preserveOaiLB, owner: owner}
		if loaded {
			if codexCookieJars.CompareAndSwap(auth.ID, existing, jar) {
				return jar
			}
		} else if actual, existed := codexCookieJars.LoadOrStore(auth.ID, jar); !existed {
			return actual.(*codexCookieJar)
		}
	}
}

// InvalidateCodexCookieJar removes all Cookie state for an auth ID.
func InvalidateCodexCookieJar(authID string) {
	if strings.TrimSpace(authID) != "" {
		codexCookieJars.Delete(authID)
	}
}

// StripCodexInternalResponseHeaders removes Cookie transport state from the
// downstream response while leaving normal protocol headers untouched.
func StripCodexInternalResponseHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	result := headers.Clone()
	result.Del("Set-Cookie")
	result.Del("Set-Cookie2")
	return result
}

func isAllowedCodexCookieURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "chatgpt.com" || host == "chat.openai.com" || host == "chatgpt-staging.com" ||
		strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
}

func isAllowedCodexCookieName(name string, preserveOaiLB bool) bool {
	name = strings.TrimSpace(name)
	if preserveOaiLB && name == "__oailb" {
		return true
	}
	for _, allowed := range codexAllowedCookieNames {
		if name == allowed {
			return true
		}
	}
	return strings.HasPrefix(name, "cf_chl_")
}

func codexOAuthAccessToken(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(value)
}

// CodexOAuthAccessToken returns the OAuth access token without considering API-key attributes.
func CodexOAuthAccessToken(auth *cliproxyauth.Auth) string { return codexOAuthAccessToken(auth) }

// CodexOAuthAccountID returns the account identity stored in an OAuth auth record.
func CodexOAuthAccountID(auth *cliproxyauth.Auth) string { return codexOAuthAccountID(auth) }

func codexOAuthAccountID(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata["account_id"].(string)
	return strings.TrimSpace(value)
}

// CodexOAuthUserAgent returns the system-specific Codex OAuth User-Agent.
// Missing or legacy system metadata is intentionally treated as macOS.
func CodexOAuthUserAgent(auth *cliproxyauth.Auth) string {
	if auth != nil {
		for key, value := range auth.Attributes {
			if strings.EqualFold(strings.TrimSpace(key), "header:User-Agent") && strings.TrimSpace(value) != "" && !strings.HasPrefix(strings.TrimSpace(value), "$") {
				return strings.TrimSpace(value)
			}
		}
	}
	return CodexSystemUserAgent(CodexOAuthClientSystem(auth))
}

// CodexOAuthClientSystem returns the system assigned to an OAuth credential.
// Missing or legacy metadata is intentionally treated as macOS.
func CodexOAuthClientSystem(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if value, ok := auth.Metadata["codex_client_system"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "windows") {
			return "windows"
		}
	}
	return "mac"
}

// IsCodexChatGPTUsageURL reports whether rawURL targets the first-party usage endpoint.
func IsCodexChatGPTUsageURL(rawURL string) bool {
	u, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil || u == nil || !isAllowedCodexCookieURL(u) {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(u.Path, "/"), codexChatGPTUsagePath)
}

// ConfigureCodexChatGPTUsageRequest applies the canonical OAuth usage headers.
// It returns true only when req targets the first-party usage endpoint for a Codex OAuth auth.
func ConfigureCodexChatGPTUsageRequest(req *http.Request, auth *cliproxyauth.Auth) bool {
	if req == nil || auth == nil || !CodexAuthUsesOAuthCookieJar(auth) || !IsCodexChatGPTUsageURL(req.URL.String()) {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+codexOAuthAccessToken(auth))
	req.Header.Set("ChatGPT-Account-ID", codexOAuthAccountID(auth))
	req.Header.Set("User-Agent", CodexOAuthUserAgent(auth))
	req.Header.Set("X-OpenAI-Codex-Luna-Reserve", "1")
	req.Header.Del("Cookie")
	return true
}

// WarmupCodexCredential performs the two first-party requests whose Set-Cookie
// responses establish the Cloudflare Cookie state used by later Codex requests.
// Response bodies are deliberately discarded; only the jar side effect matters.
func WarmupCodexCredential(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !CodexAuthUsesOAuthCookieJar(auth) {
		return
	}
	for _, endpoint := range []string{codexChatGPTUsagePath, codexChatGPTAppsPath} {
		if errWarmup := warmupCodexEndpoint(ctx, cfg, auth, endpoint); errWarmup != nil {
			log.WithError(errWarmup).WithField("auth_id", auth.ID).WithField("endpoint", endpoint).Debug("Codex cookie warmup failed")
		}
	}
}

func warmupCodexEndpoint(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, endpoint string) error {
	method := http.MethodGet
	var body io.Reader
	if endpoint == codexChatGPTAppsPath {
		method = http.MethodPost
		payload, errMarshal := json.Marshal(map[string]any{
			"app_ids": []string{
				"connector_openai_codex_document_control",
				"connector_openai_plugin_management",
				"connector_openai_safety_settings",
			},
			"include_tools": false,
		})
		if errMarshal != nil {
			return fmt.Errorf("marshal apps warmup body: %w", errMarshal)
		}
		body = strings.NewReader(string(payload))
	}
	req, errNewRequest := http.NewRequestWithContext(ctx, method, "https://chatgpt.com"+endpoint, body)
	if errNewRequest != nil {
		return errNewRequest
	}
	req.Header.Set("Authorization", "Bearer "+codexOAuthAccessToken(auth))
	req.Header.Set("ChatGPT-Account-ID", codexOAuthAccountID(auth))
	req.Header.Set("User-Agent", CodexOAuthUserAgent(auth))
	if endpoint == codexChatGPTUsagePath {
		req.Header.Set("X-OpenAI-Codex-Luna-Reserve", "1")
	} else {
		req.Header.Set("Originator", "codex-tui")
		req.Header.Set("OAI-Product-SKU", "codex")
		req.Header.Set("Content-Type", "application/json")
	}
	client := NewUtlsHTTPClient(ctx, cfg, auth, 0)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("warmup returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// StartCodexCookieRefreshLoop warms active credentials immediately and refreshes
// the usage endpoint every sixteen minutes. The auth list is read on every tick
// so enable/disable and token refresh changes take effect without restarting CPA.
func StartCodexCookieRefreshLoop(ctx context.Context, cfg *config.Config, list func() []*cliproxyauth.Auth) {
	if ctx == nil || list == nil {
		return
	}
	warm := func() {
		for _, auth := range list() {
			if auth == nil || auth.Disabled || !CodexAuthUsesOAuthCookieJar(auth) {
				continue
			}
			WarmupCodexCredential(ctx, cfg, auth)
		}
	}
	go func() {
		warm()
		ticker := time.NewTicker(codexCookieRefreshTime)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, auth := range list() {
					if auth == nil || auth.Disabled || !CodexAuthUsesOAuthCookieJar(auth) {
						continue
					}
					if errUsage := warmupCodexEndpoint(ctx, cfg, auth, codexChatGPTUsagePath); errUsage != nil {
						log.WithError(errUsage).WithField("auth_id", auth.ID).Debug("Codex usage Cookie refresh failed")
					}
				}
			}
		}
	}()
}

// CodexWebsocketCookieHeaders prepares a WSS handshake from the same HTTPS jar.
// An explicit Cookie header wins over the jar, matching the CLI transport.
func CodexWebsocketCookieHeaders(jar http.CookieJar, rawURL string, headers http.Header) http.Header {
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	target := codexWebsocketCookieURL(rawURL)
	if jar == nil || target == nil || headers.Get("Cookie") != "" {
		return headers
	}
	request := &http.Request{Header: headers}
	for _, cookie := range jar.Cookies(target) {
		request.AddCookie(cookie)
	}
	return headers
}

// StoreCodexWebsocketCookies accepts both successful and rejected handshake responses.
func StoreCodexWebsocketCookies(jar http.CookieJar, rawURL string, response *http.Response) {
	if target := codexWebsocketCookieURL(rawURL); jar != nil && target != nil && response != nil {
		jar.SetCookies(target, response.Cookies())
	}
}

func codexWebsocketCookieURL(rawURL string) *url.URL {
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "wss" {
		return nil
	}
	target.Scheme = "https"
	if !isAllowedCodexCookieURL(target) {
		return nil
	}
	return target
}

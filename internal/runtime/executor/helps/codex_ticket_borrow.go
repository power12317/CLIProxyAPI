package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const CodexTicketBorrowPath = "/v8/management/credentials/codex/ticket-borrow"
const codexBorrowLifetime = 240 * time.Second
const codexBorrowRefresh = 60 * time.Second
const codexBorrowRetry = 30 * time.Second

// CodexTicketBorrowError is request-scoped: acquisition must not silently fall
// back to ordinary tickets or trigger credential-wide cooldowns.
type CodexTicketBorrowError struct{ reason string }

func (e *CodexTicketBorrowError) Error() string {
	return "codex ticket borrowing unavailable: " + e.reason
}
func (e *CodexTicketBorrowError) StatusCode() int            { return http.StatusServiceUnavailable }
func (e *CodexTicketBorrowError) IsRequestScoped() bool      { return true }
func (e *CodexTicketBorrowError) RetryAfter() *time.Duration { d := codexBorrowRetry; return &d }
func borrowError(reason string) error                        { return &CodexTicketBorrowError{reason: reason} }

func CodexTicketBorrowEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	return cfg != nil && cfg.Codex.EffectiveTurnStateTicket().Enabled && cfg.CodexHeaderDefaults.OaiLBBorrow != nil && isCodexTurnStateTicketAccount(auth)
}

// PrepareCodexBorrowBody runs before the final user payload rules. SSE uses the
// header for the selected ticket; inherited per-turn body state is discarded.
func PrepareCodexBorrowBody(body []byte) []byte {
	out, err := sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
	if err != nil {
		return body
	}
	return out
}

type CodexBorrowedTicket struct {
	State     string
	Route     CodexTicketRoute
	ExpiresAt time.Time
	key       string
	revoked   atomic.Bool
}

func (b *CodexBorrowedTicket) valid(now time.Time) bool {
	return b != nil && !b.revoked.Load() && now.Before(b.ExpiresAt) && b.Route.valid(now)
}

type codexBorrowEntry struct {
	ticket   *CodexBorrowedTicket
	pending  chan struct{}
	retryAt  time.Time
	lastUsed time.Time
}
type codexBorrowCache struct {
	sync.Mutex
	entries map[string]*codexBorrowEntry
}

var codexBorrowedTickets = codexBorrowCache{entries: make(map[string]*codexBorrowEntry)}

// AcquireCodexBorrowedTicket is only called by a business request. It owns no
// goroutine or timer; waiters share the current credential/model acquisition.
func AcquireCodexBorrowedTicket(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model string) (*CodexBorrowedTicket, error) {
	if !CodexTicketBorrowEnabled(cfg, auth) {
		return nil, nil
	}
	if strings.TrimSpace(model) == "" {
		return nil, borrowError("final upstream model is missing")
	}
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	if err := c.Validate(); err != nil {
		return nil, borrowError("invalid source configuration")
	}
	key := codexIdentityDigest([]any{c, auth.ID, CodexOwnerFingerprint(auth), CodexOAuthAccessToken(auth), model, effectiveProxyURL(ctx, cfg, auth)})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		codexBorrowedTickets.Lock()
		entry := codexBorrowedTickets.entries[key]
		if entry == nil {
			for k, e := range codexBorrowedTickets.entries {
				if e.pending == nil && now.Sub(e.lastUsed) > 10*time.Minute {
					delete(codexBorrowedTickets.entries, k)
				}
			}
			entry = &codexBorrowEntry{}
			codexBorrowedTickets.entries[key] = entry
		}
		entry.lastUsed = now
		if entry.ticket.valid(now.Add(codexBorrowRefresh)) {
			ticket := entry.ticket
			codexBorrowedTickets.Unlock()
			return ticket, nil
		}
		if pending := entry.pending; pending != nil {
			codexBorrowedTickets.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if now.Before(entry.retryAt) {
			ticket := entry.ticket
			usable := ticket.valid(now)
			codexBorrowedTickets.Unlock()
			if usable {
				return ticket, nil
			}
			return nil, borrowError("acquisition retry is deferred for 30 seconds")
		}
		entry.pending = make(chan struct{})
		codexBorrowedTickets.Unlock()

		ticket, err := acquireCodexBorrowBundle(ctx, cfg, auth, model)
		codexBorrowedTickets.Lock()
		if err == nil {
			ticket.key = key
			entry.ticket, entry.retryAt = ticket, time.Time{}
		} else {
			entry.retryAt = time.Now().Add(codexBorrowRetry)
		}
		close(entry.pending)
		entry.pending = nil
		usable := entry.ticket.valid(time.Now())
		ticket = entry.ticket
		codexBorrowedTickets.Unlock()
		if usable {
			return ticket, nil
		}
		return nil, err
	}
}

func fetchCodexSourceBundle(ctx context.Context, cfg *config.Config, model string) (CodexTurnStateTicket, error) {
	var bundle CodexTurnStateTicket
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	secret, err := config.CodexOaiLBManagementKey(cfg)
	if err != nil {
		return bundle, borrowError("source management key unavailable")
	}
	data, _ := json.Marshal(map[string]string{"auth_id": c.SourceAuthID, "model": model})
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodPost, strings.TrimRight(c.SourceURL, "/")+CodexTicketBorrowPath, bytes.NewReader(data))
	if err != nil {
		return bundle, borrowError("invalid source URL")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return bundle, borrowError("source instance request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return bundle, borrowError(fmt.Sprintf("source instance returned HTTP %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil || len(raw) > 16384 || json.Unmarshal(raw, &bundle) != nil {
		return bundle, borrowError("invalid source bundle response")
	}
	now := time.Now()
	issued, err := CodexTicketIssuedAt(bundle.State, now)
	if err != nil || bundle.AccountID != c.SourceAuthID || bundle.Model != model || bundle.Route == nil || !bundle.Route.valid(now) || !bundle.valid(now) || !now.Before(issued.Add(time.Hour)) || bundle.ExpiresAt.After(issued.Add(time.Hour)) {
		return bundle, borrowError("source bundle is incomplete or expired")
	}
	return bundle, nil
}

func acquireCodexBorrowBundle(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model string) (*CodexBorrowedTicket, error) {
	source, err := fetchCodexSourceBundle(ctx, cfg, model)
	if err != nil {
		return nil, err
	}
	first, route, err := probeCodexBorrow(ctx, cfg, auth, model, source.State, *source.Route)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	issued, err := CodexTicketIssuedAt(first, now)
	if err != nil || first == source.State || !now.Before(issued.Add(codexBorrowLifetime)) {
		return nil, borrowError("first probe did not issue a fresh target ticket")
	}
	expires := route.deadline(issued.Add(codexBorrowLifetime))
	second, verifiedRoute, err := probeCodexBorrow(ctx, cfg, auth, model, first, route)
	if err != nil {
		return nil, err
	}
	if (second != "" && second != first) || !route.sameValues(verifiedRoute) {
		return nil, borrowError("continuation changed the ticket or cookie pair")
	}
	expires = verifiedRoute.deadline(expires)
	ticket := &CodexBorrowedTicket{State: first, Route: verifiedRoute, ExpiresAt: expires}
	if !ticket.valid(time.Now().Add(codexBorrowRefresh)) {
		return nil, borrowError("verified ticket has too little remaining lifetime")
	}
	return ticket, nil
}

func probeCodexBorrow(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, state string, route CodexTicketRoute) (string, CodexTicketRoute, error) {
	timeout := time.Duration(cfg.Codex.EffectiveTurnStateTicket().AttemptTimeoutSeconds) * time.Second
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, session, turn, err := newCodexTicketProbeRequest(attemptCtx, cfg, auth, model)
	if err != nil {
		return "", route, borrowError("probe request could not be constructed")
	}
	req.Header.Set(CodexTurnStateTicketHeader, state)
	req.Header.Set("Accept-Encoding", "identity")
	applyCodexTicketRoute(req, route)
	client := NewUtlsHTTPClient(attemptCtx, cfg, auth, timeout)
	// Each probe carries an explicit versioned pair, independent of the live jar.
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	started := time.Now()
	resp, err := client.Do(req)
	logCodexTurnStateTicketProbe(auth, req, model, session, turn, resp, time.Since(started), err)
	if err != nil {
		return "", route, borrowError("probe transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", route, borrowError(fmt.Sprintf("probe returned HTTP %d", resp.StatusCode))
	}
	result, ok := codexResponseRoute(route, resp, time.Now(), true)
	if err := readCodexBorrowComplete(resp.Body); err != nil {
		return "", route, err
	}
	if !ok || !result.valid(time.Now()) {
		return "", route, borrowError("probe cookie pair is incomplete or expired")
	}
	return strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader)), result, nil
}

func readCodexBorrowComplete(body io.Reader) error {
	const limit = 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || len(raw) > limit {
		return borrowError("probe stream read failed")
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte("\r"), []byte("\n"))
	complete := false
	for _, event := range bytes.Split(raw, []byte("\n\n")) {
		var lines [][]byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				lines = append(lines, bytes.TrimSpace(line[5:]))
			}
		}
		data := bytes.Join(lines, []byte("\n"))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(data) {
			return borrowError("probe stream contains invalid data")
		}
		kind := gjson.GetBytes(data, "type").String()
		status := gjson.GetBytes(data, "response.status").String()
		if kind == "error" || kind == "response.failed" || kind == "response.incomplete" || status == "failed" || status == "incomplete" || gjson.GetBytes(data, "error").IsObject() || gjson.GetBytes(data, "response.error").IsObject() {
			return borrowError("probe stream reported failure")
		}
		if kind == "response.completed" || kind == "response.done" {
			if status != "" && status != "completed" {
				return borrowError("probe response did not complete")
			}
			complete = true
		}
	}
	if !complete {
		return borrowError("probe stream did not complete")
	}
	return nil
}

func applyCodexTicketRoute(req *http.Request, route CodexTicketRoute) {
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != "__cflb" && c.Name != "__oailb" {
			req.AddCookie(c)
		}
	}
	req.AddCookie(&http.Cookie{Name: "__cflb", Value: route.CFLB.Value})
	req.AddCookie(&http.Cookie{Name: "__oailb", Value: route.OaiLB.Value})
}

// Apply verifies the finalized payload without modifying it, then applies the
// transport headers. A payload rule specifying another ticket must fail clearly.
func (b *CodexBorrowedTicket) Apply(req *http.Request, body []byte) error {
	if b == nil {
		return nil
	}
	if req.URL.Scheme != "https" || req.URL.Hostname() != "chatgpt.com" || req.URL.Path != codexChatGPTResponses {
		return borrowError("borrowing requires the direct ChatGPT Responses SSE endpoint")
	}
	if state := gjson.GetBytes(body, "client_metadata.x-codex-turn-state"); state.Exists() && (state.Type != gjson.String || state.String() != b.State) {
		return borrowError("final payload conflicts with borrowed ticket")
	}
	clearCodexTurnStateHeader(req.Header)
	req.Header.Set(CodexTurnStateTicketHeader, b.State)
	applyCodexTicketRoute(req, b.Route)
	return nil
}

type codexBorrowTransport struct {
	base   http.RoundTripper
	ticket *CodexBorrowedTicket
}

func (t *codexBorrowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A concurrent renewal does not invalidate an already selected, still valid generation.
	if !t.ticket.valid(time.Now()) {
		return nil, borrowError("selected ticket expired or was revoked before send")
	}
	copy := req.Clone(req.Context())
	applyCodexTicketRoute(copy, t.ticket.Route)
	return t.base.RoundTrip(copy)
}
func (b *CodexBorrowedTicket) ConfigureClient(client *http.Client) {
	if b == nil {
		return
	}
	client.Transport = &codexBorrowTransport{base: client.Transport, ticket: b}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
}

// ObserveResponse revokes only the generation used by this business exchange.
// It neither adopts unverified state nor performs an immediate acquisition.
func (b *CodexBorrowedTicket) ObserveResponse(resp *http.Response) {
	if b == nil || resp == nil {
		return
	}
	state := strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader))
	route, ok := codexResponseRoute(b.Route, resp, time.Now(), true)
	changed := (state != "" && state != b.State) || !ok || !route.sameValues(b.Route) || route.deadline(b.ExpiresAt).Before(b.ExpiresAt)
	if !changed {
		return
	}
	b.revoked.Store(true)
}

var _ error = (*CodexTicketBorrowError)(nil)

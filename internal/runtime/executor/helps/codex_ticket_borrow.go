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
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const CodexTicketBorrowPath = "/v0/management/codex/oailb/borrow"
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
	authFile  string
	model     string
	revoked   atomic.Bool
}

func (b *CodexBorrowedTicket) valid(now time.Time) bool {
	return b != nil && !b.revoked.Load() && now.Before(b.ExpiresAt) && b.Route.valid(now)
}

type codexBorrowEntry struct {
	ticket        *CodexBorrowedTicket
	pending       chan struct{}
	retryAt       time.Time
	lastUsed      time.Time
	lastFailure   string
	lastRequestID string
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
	trace := newCodexBorrowLog(ctx, auth, model)
	if strings.TrimSpace(model) == "" {
		trace.event(log.WarnLevel, "ACQUIRE", "final upstream model is missing; business request not sent")
		return nil, borrowError("final upstream model is missing")
	}
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	if err := c.Validate(); err != nil {
		trace.event(log.WarnLevel, "ACQUIRE", "invalid source configuration; business request not sent")
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
			trace.event(log.InfoLevel, "REUSE", codexBorrowTicketDetail(ticket))
			return ticket, nil
		}
		if pending := entry.pending; pending != nil {
			requestID := entry.lastRequestID
			codexBorrowedTickets.Unlock()
			trace.event(log.InfoLevel, "WAIT", "sharing acquisition request: "+requestID)
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				trace.event(log.WarnLevel, "WAIT", codexBorrowIOReason(ctx.Err())+"; business request not sent")
				return nil, ctx.Err()
			}
		}
		if now.Before(entry.retryAt) {
			ticket := entry.ticket
			usable := ticket.valid(now)
			reason := codexBorrowRetryReason(entry.retryAt, now, entry.lastFailure, entry.lastRequestID)
			codexBorrowedTickets.Unlock()
			if usable {
				trace.event(log.InfoLevel, "REUSE", codexBorrowTicketDetail(ticket)+" | "+reason)
				return ticket, nil
			}
			trace.event(log.WarnLevel, "WAIT", reason+"; business request not sent")
			return nil, borrowError(reason)
		}
		entry.pending = make(chan struct{})
		entry.lastRequestID = trace.requestID
		codexBorrowedTickets.Unlock()

		trace.event(log.InfoLevel, "ACQUIRE", fmt.Sprintf("source=%q", codexBorrowSourceURL(c.SourceURL)))
		ticket, err := acquireCodexBorrowBundle(ctx, cfg, auth, model, trace)
		codexBorrowedTickets.Lock()
		if err == nil {
			ticket.key = key
			ticket.authFile, ticket.model = trace.authFile, model
			entry.ticket, entry.retryAt = ticket, time.Time{}
			entry.lastFailure = ""
		} else {
			entry.retryAt = time.Now().Add(codexBorrowRetry)
			entry.lastFailure = codexBorrowFailureReason(err)
		}
		close(entry.pending)
		entry.pending = nil
		usable := entry.ticket.valid(time.Now())
		ticket = entry.ticket
		codexBorrowedTickets.Unlock()
		if err != nil {
			disposition := "business request not sent"
			if usable {
				disposition = "retaining unexpired verified ticket"
			}
			trace.event(log.WarnLevel, "ACQUIRE", codexBorrowFailureReason(err)+"; retry in 30 seconds; "+disposition)
		} else {
			trace.event(log.InfoLevel, "READY", codexBorrowTicketDetail(ticket))
		}
		if usable {
			if err != nil {
				trace.event(log.InfoLevel, "REUSE", codexBorrowTicketDetail(ticket))
			}
			return ticket, nil
		}
		return nil, err
	}
}

func fetchCodexSourceBundle(ctx context.Context, cfg *config.Config, model string, trace codexBorrowLog) (bundle CodexTurnStateTicket, errResult error) {
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	started := time.Now()
	var req *http.Request
	var resp *http.Response
	defer func() {
		detail := ""
		// SOURCE reports the returned snapshot model, not an upstream inference.
		trace.responseModel = codexBorrowRedactErrorText(bundle.Model, []string{c.SourceAuthID, c.SourceAuthFile, c.SourceManagementKey})
		if bundle.Route != nil {
			trace.node = CodexOaiLBNode(bundle.Route.OaiLB.Value)
			detail = fmt.Sprintf(" | remaining=%ds", max(int64(bundle.Route.deadline(bundle.ExpiresAt).Sub(time.Now())/time.Second), 0))
		}
		trace.request("SOURCE", req, resp, len(bundle.State), time.Since(started), detail, errResult)
	}()
	secret, err := config.CodexOaiLBManagementKey(cfg)
	if err != nil {
		return bundle, borrowError("source management key unavailable")
	}
	data, _ := json.Marshal(map[string]string{"auth_id": c.SourceAuthID, "model": model})
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err = http.NewRequestWithContext(fetchCtx, http.MethodPost, strings.TrimRight(c.SourceURL, "/")+CodexTicketBorrowPath, bytes.NewReader(data))
	if err != nil {
		return bundle, borrowError("invalid source URL")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.Do(req)
	if err != nil {
		return bundle, borrowError("source instance request failed: " + codexBorrowIOReason(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return bundle, codexBorrowHTTPFailure("source instance", resp, codexBorrowErrorSecrets(cfg, nil, req, resp), nil)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	var result CodexOaiLBBorrowResponse
	if err != nil {
		return bundle, borrowError("source response read failed: " + codexBorrowIOReason(err))
	}
	if len(raw) > 16384 || json.Unmarshal(raw, &result) != nil {
		return bundle, borrowError("invalid source bundle response")
	}
	if !result.Available {
		return bundle, borrowError("source ticket bundle is unavailable")
	}
	if result.CodexTurnStateTicket == nil {
		return bundle, borrowError("source response has no ticket snapshot")
	}
	bundle = *result.CodexTurnStateTicket
	if bundle.Route == nil || result.Value != bundle.Route.OaiLB.Value || result.CFLB != bundle.Route.CFLB.Value {
		return bundle, borrowError("source response cookie pair is inconsistent")
	}
	now := time.Now()
	issued, err := CodexTicketIssuedAt(bundle.State, now)
	if err != nil {
		return bundle, borrowError("source " + err.Error())
	}
	if bundle.AccountID != c.SourceAuthID || bundle.Model != model {
		return bundle, borrowError("source credential or model does not match the request")
	}
	if !bundle.Route.valid(now) {
		return bundle, borrowError("source " + codexBorrowRouteFailure(*bundle.Route, now))
	}
	if !bundle.valid(now) || !now.Before(issued.Add(time.Hour)) || bundle.ExpiresAt.After(issued.Add(time.Hour)) {
		return bundle, borrowError("source ticket expired or has invalid length/lifetime metadata")
	}
	return bundle, nil
}

func acquireCodexBorrowBundle(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model string, trace codexBorrowLog) (*CodexBorrowedTicket, error) {
	source, err := fetchCodexSourceBundle(ctx, cfg, model, trace)
	if err != nil {
		return nil, codexBorrowStageError("source", err)
	}
	first, route, err := probeCodexBorrow(ctx, cfg, auth, model, source.State, *source.Route, trace, "EXCHANGE")
	if err != nil {
		return nil, codexBorrowStageError("exchange", err)
	}
	now := time.Now()
	issued, err := CodexTicketIssuedAt(first, now)
	if first == "" {
		return nil, borrowError("exchange: response has no target ticket")
	}
	if err != nil {
		return nil, borrowError("exchange: " + err.Error())
	}
	if first == source.State {
		return nil, borrowError("exchange: response repeated the source ticket")
	}
	if !now.Before(issued.Add(codexBorrowLifetime)) {
		return nil, borrowError(fmt.Sprintf("exchange: target ticket is already expired (age=%ds, limit=240s)", int64(now.Sub(issued)/time.Second)))
	}
	expires := route.deadline(issued.Add(codexBorrowLifetime))
	second, verifiedRoute, err := probeCodexBorrow(ctx, cfg, auth, model, first, route, trace, "VERIFY")
	if err != nil {
		return nil, codexBorrowStageError("verify", err)
	}
	if second != "" && second != first {
		return nil, borrowError(fmt.Sprintf("verify: continuation returned a different ticket (%d/%d)", len(first), len(second)))
	}
	if !route.sameValues(verifiedRoute) {
		return nil, borrowError(fmt.Sprintf("verify: continuation changed cookie values (__cflb=%t, __oailb=%t)", route.CFLB.Value != verifiedRoute.CFLB.Value, route.OaiLB.Value != verifiedRoute.OaiLB.Value))
	}
	expires = verifiedRoute.deadline(expires)
	ticket := &CodexBorrowedTicket{State: first, Route: verifiedRoute, ExpiresAt: expires}
	if !ticket.valid(time.Now().Add(codexBorrowRefresh)) {
		return nil, borrowError(fmt.Sprintf("verify: ticket has too little remaining lifetime (remaining=%ds, required>60s)", max(int64(expires.Sub(time.Now())/time.Second), 0)))
	}
	return ticket, nil
}

func probeCodexBorrow(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, state string, route CodexTicketRoute, trace codexBorrowLog, stage string) (outState string, outRoute CodexTicketRoute, errResult error) {
	started := time.Now()
	var resp *http.Response
	timeout := time.Duration(cfg.Codex.EffectiveTurnStateTicket().AttemptTimeoutSeconds) * time.Second
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, session, turn, err := newCodexTicketProbeRequest(attemptCtx, cfg, auth, model)
	trace.sessionID, trace.turnID = session, turn
	defer func() {
		length := 0
		if req != nil {
			trace.node = CodexOaiLBNodeForExchange(req.Header, resp)
		}
		if resp != nil {
			length = len(strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader)))
		}
		trace.request(stage, req, resp, length, time.Since(started), codexBorrowResponseDetail(state, resp), errResult)
	}()
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
	resp, err = client.Do(req)
	if err != nil {
		return "", route, borrowError("probe transport failed: " + codexBorrowIOReason(err))
	}
	defer func() { _ = resp.Body.Close() }()
	secrets := codexBorrowErrorSecrets(cfg, auth, req, resp)
	if resp.StatusCode != http.StatusOK {
		return "", route, codexBorrowHTTPFailure("probe", resp, secrets, &trace)
	}
	result, ok := codexResponseRoute(route, resp, time.Now(), true)
	if err := readCodexBorrowCompleteObserved(resp.Body, &trace, secrets); err != nil {
		return "", route, err
	}
	if !ok || !result.valid(time.Now()) {
		reason := codexBorrowRouteFailure(result, time.Now())
		if reason == "" {
			reason = "response updated only one routing cookie; both __cflb and __oailb are required together"
		}
		return "", route, borrowError("probe " + reason)
	}
	return strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader)), result, nil
}

func readCodexBorrowComplete(body io.Reader, secrets ...string) error {
	return readCodexBorrowCompleteObserved(body, nil, secrets)
}

func readCodexBorrowCompleteObserved(body io.Reader, trace *codexBorrowLog, secrets []string) error {
	const limit = 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if len(raw) > limit {
		return borrowError("probe stream exceeds 1048576 bytes")
	}
	// Preserve metadata already received even if the body later fails to read.
	errComplete := parseCodexBorrowComplete(raw, trace, secrets)
	if err != nil {
		return borrowError(fmt.Sprintf("probe stream read failed: %s (%d bytes)", codexBorrowIOReason(err), len(raw)))
	}
	return errComplete
}

func parseCodexBorrowComplete(raw []byte, trace *codexBorrowLog, secrets []string) error {
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
		trace.observeResponseModel(data, secrets)
		kind := gjson.GetBytes(data, "type").String()
		status := gjson.GetBytes(data, "response.status").String()
		if kind == "error" || kind == "response.failed" || kind == "response.incomplete" || status == "failed" || status == "incomplete" || gjson.GetBytes(data, "error").IsObject() || gjson.GetBytes(data, "response.error").IsObject() {
			return borrowError("probe stream reported failure: " + codexBorrowStreamFailure(data, secrets...))
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
		t.ticket.businessLog(req).event(log.WarnLevel, "SEND", "selected ticket expired or was revoked; business request not sent")
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
	if b.revoked.Swap(true) {
		return
	}
	reason := "business response revoked the selected ticket"
	if cookieFailure := codexBorrowRouteFailure(route, time.Now()); cookieFailure != "" {
		reason += "; " + cookieFailure
	} else if !ok {
		reason += "; response updated only one routing cookie"
	} else if route.deadline(b.ExpiresAt).Before(b.ExpiresAt) {
		reason += "; cookie lifetime shortened"
	}
	b.businessLog(resp.Request).event(log.WarnLevel, "REVOKE", reason+codexBorrowResponseDetail(b.State, resp)+"; next business request must acquire again")
}

var _ error = (*CodexTicketBorrowError)(nil)

package helps

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// CodexRouteCookie retains the original deadline, never a refreshed Max-Age.
type CodexRouteCookie struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type CodexTicketRoute struct {
	CFLB  CodexRouteCookie `json:"__cflb"`
	OaiLB CodexRouteCookie `json:"__oailb"`
}

func (r CodexTicketRoute) valid(now time.Time) bool {
	for _, c := range []CodexRouteCookie{r.CFLB, r.OaiLB} {
		if c.Value == "" || (!c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)) {
			return false
		}
		if (&http.Cookie{Name: "route", Value: c.Value}).Valid() != nil {
			return false
		}
	}
	return true
}
func (r CodexTicketRoute) deadline(limit time.Time) time.Time {
	for _, c := range []CodexRouteCookie{r.CFLB, r.OaiLB} {
		if !c.ExpiresAt.IsZero() && c.ExpiresAt.Before(limit) {
			limit = c.ExpiresAt
		}
	}
	return limit
}
func (r CodexTicketRoute) sameValues(other CodexTicketRoute) bool {
	return r.CFLB.Value == other.CFLB.Value && r.OaiLB.Value == other.OaiLB.Value
}

// CodexTicketIssuedAt reads the timestamp without claiming to verify the signature.
func CodexTicketIssuedAt(state string, now time.Time) (time.Time, error) {
	if len(state) != config.DefaultCodexTurnStateTicketTargetLength {
		return time.Time{}, errors.New("invalid ticket length")
	}
	raw, err := base64.URLEncoding.Strict().DecodeString(state)
	if err != nil {
		raw, err = base64.RawURLEncoding.Strict().DecodeString(state)
	}
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return time.Time{}, errors.New("invalid ticket structure")
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds < 1577836800 || seconds > uint64(now.Add(30*time.Second).Unix()) {
		return time.Time{}, errors.New("invalid ticket timestamp")
	}
	return time.Unix(int64(seconds), 0), nil
}

func codexRouteCookie(cookie *http.Cookie, now time.Time) CodexRouteCookie {
	out := CodexRouteCookie{Value: cookie.Value, ExpiresAt: cookie.Expires}
	if cookie.MaxAge < 0 {
		return CodexRouteCookie{}
	}
	if cookie.MaxAge > 0 {
		out.ExpiresAt = now.Add(time.Duration(cookie.MaxAge) * time.Second)
	}
	if cookie.Name == "__oailb" {
		parts := strings.Split(cookie.Value, ".")
		if len(parts) == 3 {
			if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				if exp := gjson.GetBytes(raw, "exp").Int(); exp > 0 {
					expiry := time.Unix(exp, 0)
					if out.ExpiresAt.IsZero() || expiry.Before(out.ExpiresAt) {
						out.ExpiresAt = expiry
					}
				}
			}
		}
	}
	return out
}

// A response can update only one cookie; it remains paired with the cookie sent
// in that exchange. Strict B verification additionally requires complete updates.
func codexResponseRoute(seed CodexTicketRoute, resp *http.Response, now time.Time, strict bool) (CodexTicketRoute, bool) {
	route := seed
	seenCF, seenOAI := false, false
	if resp != nil {
		for _, c := range resp.Cookies() {
			switch c.Name {
			case "__cflb":
				route.CFLB = codexRouteCookie(c, now)
				seenCF = true
			case "__oailb":
				route.OaiLB = codexRouteCookie(c, now)
				seenOAI = true
			}
		}
	}
	return route, route.valid(now) && (!strict || seenCF == seenOAI)
}

type codexSourceRouteKey struct{}

// This wrapper runs after http.Client has attached its jar cookies. Capturing
// the outgoing exchange here avoids pairing a ticket with a later jar version.
type codexSourceRouteTransport struct {
	base http.RoundTripper
	jar  *codexCookieJar
}

func (t *codexSourceRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isAllowedCodexCookieURL(req.URL) {
		return t.base.RoundTrip(req)
	}
	route := CodexTicketRoute{}
	for _, c := range req.Cookies() {
		if c.Name != "__cflb" && c.Name != "__oailb" {
			continue
		}
		value := t.jar.routeCookie(c)
		if c.Name == "__cflb" {
			route.CFLB = value
		} else {
			route.OaiLB = value
		}
	}
	sent := req.Clone(context.WithValue(req.Context(), codexSourceRouteKey{}, route))
	resp, err := t.base.RoundTrip(sent)
	if resp != nil {
		resp.Request = sent
	}
	return resp, err
}
func sourceResponseRoute(resp *http.Response, now time.Time) (CodexTicketRoute, bool) {
	seed := CodexTicketRoute{}
	if resp != nil && resp.Request != nil {
		seed, _ = resp.Request.Context().Value(codexSourceRouteKey{}).(CodexTicketRoute)
	}
	return codexResponseRoute(seed, resp, now, false)
}

func newCodexSourceTicket(auth *cliproxyauth.Auth, model, state string, resp *http.Response, now time.Time) CodexTurnStateTicket {
	issued, err := CodexTicketIssuedAt(state, now)
	if err != nil {
		issued = now
	}
	ticket := CodexTurnStateTicket{AccountID: auth.ID, Owner: CodexOwnerFingerprint(auth), Model: strings.TrimSpace(model), State: state, Length: len(state), CapturedAt: issued, ExpiresAt: issued.Add(time.Hour), Attempts: 1}
	if route, ok := sourceResponseRoute(resp, now); ok {
		ticket.Route = &route
	}
	if previous := codexTurnStateTicketForAuth(auth, model); previous != nil && (previous.Owner == "" || previous.Owner == ticket.Owner) {
		if previous.State != state && ticket.Route == nil && previous.valid(now) && previous.Route != nil && previous.Route.valid(now) {
			return *previous
		}
		if previous.State != state {
			return ticket
		}
		// Re-observation cannot turn an old ticket into a newly issued ticket.
		ticket.CapturedAt, ticket.ExpiresAt = previous.CapturedAt, previous.ExpiresAt
		if previous.Route != nil && (previous.Route.valid(now) || ticket.Route == nil || previous.Route.sameValues(*ticket.Route)) {
			ticket.Route = previous.Route
		}
	}
	return ticket
}

// ExportCodexSourceTicket reads a persisted snapshot and never probes upstream.
func ExportCodexSourceTicket(auth *cliproxyauth.Auth, model string, now time.Time) (CodexTurnStateTicket, error) {
	t := codexTurnStateTicketForAuth(auth, model)
	if auth == nil || auth.Disabled || !isCodexTurnStateTicketAccount(auth) || (auth.Status != "" && auth.Status != cliproxyauth.StatusActive) || !t.valid(now) || t.Owner != CodexOwnerFingerprint(auth) || t.Route == nil || !t.Route.valid(now) {
		return CodexTurnStateTicket{}, errors.New("source ticket bundle unavailable")
	}
	issued, err := CodexTicketIssuedAt(t.State, now)
	if err != nil || !now.Before(issued.Add(time.Hour)) {
		return CodexTurnStateTicket{}, errors.New("source ticket expired or malformed")
	}
	if limit := issued.Add(time.Hour); limit.Before(t.ExpiresAt) {
		t.ExpiresAt = limit
	}
	return *t, nil
}

// CodexOaiLBBorrowResponse extends the original available/value/cflb envelope.
// The embedded model-scoped snapshot is required by the SSE borrower; a legacy
// cookie-only response cannot be promoted into a verified ticket bundle.
type CodexOaiLBBorrowResponse struct {
	Available        bool   `json:"available"`
	Value            string `json:"value,omitempty"`
	CFLB             string `json:"cflb,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds,omitempty"`
	*CodexTurnStateTicket
}

func NewCodexOaiLBBorrowResponse(ticket CodexTurnStateTicket, now time.Time) CodexOaiLBBorrowResponse {
	if !ticket.valid(now) || ticket.Route == nil || !ticket.Route.valid(now) {
		return CodexOaiLBBorrowResponse{}
	}
	return CodexOaiLBBorrowResponse{Available: true, Value: ticket.Route.OaiLB.Value, CFLB: ticket.Route.CFLB.Value, RemainingSeconds: int64(ticket.Route.deadline(ticket.ExpiresAt).Sub(now) / time.Second), CodexTurnStateTicket: &ticket}
}

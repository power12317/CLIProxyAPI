package helps

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func borrowTestState(issued time.Time, nonce byte) string {
	raw := make([]byte, 585)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	raw[9] = nonce
	return base64.URLEncoding.EncodeToString(raw)
}
func borrowTestRoute() CodexTicketRoute {
	return CodexTicketRoute{CFLB: CodexRouteCookie{Value: "source-cf"}, OaiLB: CodexRouteCookie{Value: "source-oai"}}
}

const borrowTestComplete = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

type borrowFixture struct {
	cfg        *config.Config
	auth       *cliproxyauth.Auth
	ctx        context.Context
	seed       CodexTurnStateTicket
	fetches    atomic.Int32
	probes     atomic.Int32
	failSource atomic.Bool
	shot       func(*http.Request, int32) (*http.Response, error)
}

func newBorrowFixture(t *testing.T) *borrowFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	route := borrowTestRoute()
	f := &borrowFixture{auth: ticketProbeTestAuth(t.Name(), "target@example.com", "target-account")}
	f.seed = CodexTurnStateTicket{AccountID: "source-auth", Model: "gpt-6-astra", State: borrowTestState(now.Add(-50*time.Minute), 1), Length: 780, CapturedAt: now.Add(-50 * time.Minute), ExpiresAt: now.Add(10 * time.Minute), Route: &route}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		if r.URL.Path != CodexTicketBorrowPath || r.Header.Get("Authorization") != "Bearer management-secret" {
			t.Error("incorrect donor protocol or authorization")
			w.WriteHeader(400)
			return
		}
		if f.failSource.Load() {
			w.WriteHeader(503)
			return
		}
		var request struct {
			AuthID string `json:"auth_id"`
			Model  string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.AuthID != f.seed.AccountID {
			t.Error("incorrect donor selection")
			w.WriteHeader(400)
			return
		}
		bundle := f.seed
		bundle.Model = request.Model
		_ = json.NewEncoder(w).Encode(NewCodexOaiLBBorrowResponse(bundle, time.Now()))
	}))
	t.Cleanup(source.Close)
	t.Cleanup(func() { InvalidateCodexCookieJar(f.auth.ID) })
	f.cfg = &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}, CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: source.URL, SourceAuthID: "source-auth", SourceManagementKey: "management-secret"}}}
	f.ctx = context.WithValue(t.Context(), "cliproxy.roundtripper", codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := f.probes.Add(1)
		if req.Header.Get("Authorization") != "Bearer "+f.auth.Metadata["access_token"].(string) || req.Header.Get("Chatgpt-Account-Id") != "target-account" {
			t.Error("probe did not use target OAuth identity")
		}
		cf, errCF := req.Cookie("__cflb")
		oai, errOAI := req.Cookie("__oailb")
		if errCF != nil || errOAI != nil || cf.Value != "source-cf" || oai.Value != "source-oai" {
			t.Error("probe mixed route cookies")
		}
		raw, _ := io.ReadAll(req.Body)
		if gjson.GetBytes(raw, "input.0.content.0.text").String() != "ping" || !strings.Contains(gjson.GetBytes(raw, "instructions").String(), "pong") {
			t.Error("unexpected probe payload")
		}
		if f.shot != nil {
			return f.shot(req, n)
		}
		header := make(http.Header)
		if req.Header.Get(CodexTurnStateTicketHeader) == f.seed.State {
			header.Set(CodexTurnStateTicketHeader, borrowTestState(time.Now().Add(-5*time.Second), byte(n+1)))
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(borrowTestComplete)), Request: req}, nil
	}))
	return f
}
func (f *borrowFixture) acquire(t *testing.T) *CodexBorrowedTicket {
	t.Helper()
	ticket, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestCodexBorrowRequestDrivenBoundariesAndReuse(t *testing.T) {
	f := newBorrowFixture(t)
	h := NewCodexTurnStateTicketHarvester(CodexTurnStateTicketHarvesterOptions{Config: func() *config.Config { return f.cfg }, List: func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{f.auth} }, Update: func(context.Context, *cliproxyauth.Auth, *cliproxyauth.Auth) error {
		t.Error("borrower started source harvesting")
		return nil
	}})
	h.refresh(f.ctx)
	if f.fetches.Load() != 0 || f.probes.Load() != 0 {
		t.Fatal("borrower performed idle acquisition")
	}
	ticket := f.acquire(t)
	if f.fetches.Load() != 1 || f.probes.Load() != 2 || ticket.State == f.seed.State {
		t.Fatal("did not acquire and verify a target-owned ticket")
	}
	issued, _ := CodexTicketIssuedAt(ticket.State, time.Now())
	if !ticket.ExpiresAt.Equal(issued.Add(240 * time.Second)) {
		t.Fatal("validation restarted the lifetime")
	}
	ticket.ExpiresAt = time.Now().Add(61 * time.Second)
	if got := f.acquire(t); got != ticket || f.fetches.Load() != 1 {
		t.Fatal("61 seconds should reuse")
	}
	ticket.ExpiresAt = time.Now().Add(60 * time.Second)
	next := f.acquire(t)
	if next == ticket || f.fetches.Load() != 2 || f.probes.Load() != 4 {
		t.Fatal("60 seconds did not renew using the same 50-minute-old source")
	}
	next.ExpiresAt = time.Now()
	if got := f.acquire(t); got == next || f.fetches.Load() != 3 {
		t.Fatal("expired target was reused")
	}
}

func TestCodexBorrowFailureUsesOnlyUnexpiredVerifiedTicket(t *testing.T) {
	f := newBorrowFixture(t)
	ticket := f.acquire(t)
	ticket.ExpiresAt = time.Now().Add(50 * time.Second)
	f.failSource.Store(true)
	if got := f.acquire(t); got != ticket {
		t.Fatal("unexpired target was lost after failed renewal")
	}
	if got := f.acquire(t); got != ticket || f.fetches.Load() != 2 {
		t.Fatal("request-triggered retry throttle failed")
	}
	ticket.ExpiresAt = time.Now()
	if got, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra"); got != nil || err == nil {
		t.Fatal("expired target revived on acquisition failure")
	}
	codexBorrowedTickets.Lock()
	codexBorrowedTickets.entries[ticket.key].retryAt = time.Now().Add(-time.Second)
	codexBorrowedTickets.Unlock()
	f.failSource.Store(false)
	if f.acquire(t) == ticket || f.fetches.Load() != 3 {
		t.Fatal("later business request did not retry")
	}
}

func TestCodexBorrowSingleflightAndCredentialModelIsolation(t *testing.T) {
	f := newBorrowFixture(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *CodexBorrowedTicket, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ticket, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
			if err != nil {
				t.Error(err)
			}
			results <- ticket
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first *CodexBorrowedTicket
	for got := range results {
		if first == nil {
			first = got
		}
		if got != first {
			t.Fatal("concurrent requests did not share one generation")
		}
	}
	if f.fetches.Load() != 1 || f.probes.Load() != 2 {
		t.Fatal("concurrent acquisition was not coalesced")
	}
	other := f.auth.Clone()
	other.ID += "-windows"
	other.Metadata["codex_client_system"] = "windows"
	if _, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, other, "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	if f.fetches.Load() != 3 || f.probes.Load() != 6 {
		t.Fatal("credentials or models shared a cache")
	}
}

func TestCodexBorrowValidationRejectsInvalidExchanges(t *testing.T) {
	for _, mode := range []string{"seed-echo", "missing-state", "expired-target", "incomplete", "error-event", "http-error", "changed-state", "changed-route", "partial-route", "deleted-route", "cookie-expired", "not-fernet"} {
		t.Run(mode, func(t *testing.T) {
			f := newBorrowFixture(t)
			f.shot = func(req *http.Request, n int32) (*http.Response, error) {
				h := make(http.Header)
				status := 200
				body := borrowTestComplete
				if n == 1 {
					h.Set(CodexTurnStateTicketHeader, borrowTestState(time.Now(), 2))
				}
				switch mode {
				case "seed-echo":
					h.Set(CodexTurnStateTicketHeader, f.seed.State)
				case "missing-state":
					h.Del(CodexTurnStateTicketHeader)
				case "expired-target":
					h.Set(CodexTurnStateTicketHeader, borrowTestState(time.Now().Add(-241*time.Second), 2))
				case "incomplete":
					body = "data: {\"type\":\"response.created\"}\n\n"
				case "error-event":
					body = "data: {\"type\":\"error\"}\n\n" + borrowTestComplete
				case "http-error":
					status = 429
				case "changed-state":
					if n == 2 {
						h.Set(CodexTurnStateTicketHeader, borrowTestState(time.Now(), 3))
					}
				case "changed-route":
					if n == 2 {
						h.Add("Set-Cookie", "__cflb=new; Secure; Path=/")
						h.Add("Set-Cookie", "__oailb=new; Secure; Path=/")
					}
				case "partial-route":
					h.Add("Set-Cookie", "__cflb=new; Secure; Path=/")
				case "deleted-route":
					h.Add("Set-Cookie", "__cflb=; Max-Age=0; Path=/")
				case "cookie-expired":
					h.Add("Set-Cookie", "__cflb=x; Expires=Wed, 01 Jan 2020 00:00:00 GMT; Path=/")
					h.Add("Set-Cookie", "__oailb=x; Path=/")
				case "not-fernet":
					h.Set(CodexTurnStateTicketHeader, strings.Repeat("x", 780))
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}
			if got, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra"); got != nil || err == nil {
				t.Fatal("invalid exchange accepted")
			}
			before := f.probes.Load()
			_, _ = AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
			if f.probes.Load() != before {
				t.Fatal("failed exchange retried before throttle elapsed")
			}
		})
	}
}

func TestCodexBorrowBusinessCookieOverlayRevocationAndLongStream(t *testing.T) {
	f := newBorrowFixture(t)
	ticket := f.acquire(t)
	target, _ := url.Parse(codexTurnStateTicketProbeURL)
	jar := CodexCookieJarForAuth(f.auth)
	jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "wrong-cf"}, {Name: "__oailb", Value: "wrong-oai"}})
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodPost, target.String(), strings.NewReader(`{"model":"gpt-6-astra"}`))
	if err := ticket.Apply(req, []byte(`{"model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Jar: jar, Transport: codexRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		names := map[string]int{}
		for _, c := range r.Cookies() {
			names[c.Name]++
			if c.Name == "__cflb" && c.Value != "source-cf" || c.Name == "__oailb" && c.Value != "source-oai" {
				t.Error("jar replaced the selected pair")
			}
		}
		if names["__cflb"] != 1 || names["__oailb"] != 1 {
			t.Error("duplicate routing cookies")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(borrowTestComplete)), Request: r}, nil
	})}
	ticket.ConfigureClient(client)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ticket.ExpiresAt = time.Now().Add(-time.Second)
	if raw, err := io.ReadAll(resp.Body); err != nil || string(raw) != borrowTestComplete {
		t.Fatal("in-progress response interrupted at ticket expiry")
	}
	_ = resp.Body.Close()
	if _, err = client.Do(req); err == nil || calls != 1 {
		t.Fatal("expired ticket reached the transport")
	}
	fresh := f.acquire(t)
	// An old response cannot revoke a newer generation.
	ticket.ObserveResponse(&http.Response{Header: http.Header{CodexTurnStateTicketHeader: []string{"different"}}})
	if f.acquire(t) != fresh {
		t.Fatal("old generation revoked the new ticket")
	}
	fresh.ObserveResponse(&http.Response{Header: http.Header{CodexTurnStateTicketHeader: []string{"different"}}})
	if f.probes.Load() != 4 {
		t.Fatal("business observation acquired eagerly")
	}
	if f.acquire(t) == fresh || f.probes.Load() != 6 {
		t.Fatal("new business request did not renew revoked ticket")
	}
}

func TestCodexBorrowFinalPayloadConflict(t *testing.T) {
	f := newBorrowFixture(t)
	ticket := f.acquire(t)
	raw := []byte(`{"client_metadata":{"x-codex-turn-state":"old-turn"},"input":"business"}`)
	clean := PrepareCodexBorrowBody(raw)
	if gjson.GetBytes(clean, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatal("old turn body was retained")
	}
	req, _ := http.NewRequest(http.MethodPost, codexTurnStateTicketProbeURL, nil)
	if err := ticket.Apply(req, raw); err == nil {
		t.Fatal("final user payload was silently overridden")
	}
	if err := ticket.Apply(req, clean); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(clean, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatal("filtered body ticket restored after finalization")
	}
}

func TestCodexBorrowSSECompletion(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{borrowTestComplete, true}, {"\xef\xbb\xbf" + strings.ReplaceAll(borrowTestComplete, "\n", "\r\n"), true},
		{"data: [DONE]\n\n", false}, {borrowTestComplete + "data: {\"type\":\"response.failed\"}\n\n", false},
		{"data: {\"type\":\"response.completed\",\n" + "data: \"response\":{\"status\":\"completed\"}}\n\n", true},
	} {
		if err := readCodexBorrowComplete(strings.NewReader(tc.raw)); (err == nil) != tc.ok {
			t.Errorf("completion=%v want=%v: %s", err, tc.ok, fmt.Sprintf("%q", tc.raw))
		}
	}
}

func TestCodexBorrowAlreadySelectedGenerationSurvivesConcurrentRenewal(t *testing.T) {
	f := newBorrowFixture(t)
	old := f.acquire(t)
	old.ExpiresAt = time.Now().Add(50 * time.Second)
	next := f.acquire(t)
	if old == next {
		t.Fatal("renewal did not change the generation")
	}
	calls := 0
	client := &http.Client{Transport: codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("done")), Request: req}, nil
	})}
	old.ConfigureClient(client)
	req, _ := http.NewRequest(http.MethodPost, codexTurnStateTicketProbeURL, nil)
	if err := old.Apply(req, []byte(`{"model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("still-valid selected generation rejected", err)
	}
	_ = resp.Body.Close()
	old.ObserveResponse(&http.Response{Header: http.Header{CodexTurnStateTicketHeader: []string{"different"}}})
	if _, err := client.Do(req); err == nil || calls != 1 {
		t.Fatal("revoked old generation reached upstream")
	}
	if f.acquire(t) != next {
		t.Fatal("old generation revoked the replacement")
	}
}

func TestCodexBorrowOriginalEndpointRejectsIncompleteAndMismatchedEnvelope(t *testing.T) {
	for _, kind := range []string{"unavailable", "cookie-only", "unwrapped-ticket", "mismatched-cookie"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			route := borrowTestRoute()
			ticket := CodexTurnStateTicket{AccountID: "donor", Model: "model", State: borrowTestState(now, 1), Length: 780, CapturedAt: now, ExpiresAt: now.Add(time.Hour), Route: &route}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/management/codex/oailb/borrow" {
					t.Errorf("unexpected source endpoint: %s", r.URL.Path)
				}
				switch kind {
				case "unavailable":
					_, _ = io.WriteString(w, `{"available":false}`)
				case "cookie-only":
					_, _ = io.WriteString(w, `{"available":true,"value":"source-oai","cflb":"source-cf"}`)
				case "unwrapped-ticket":
					_ = json.NewEncoder(w).Encode(ticket)
				case "mismatched-cookie":
					response := NewCodexOaiLBBorrowResponse(ticket, now)
					response.CFLB = "another-version"
					_ = json.NewEncoder(w).Encode(response)
				}
			}))
			defer source.Close()
			cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: source.URL, SourceAuthID: "donor", SourceManagementKey: "key"}}}
			if _, err := fetchCodexSourceBundle(t.Context(), cfg, "model", newCodexBorrowLog(t.Context(), nil, "model")); err == nil {
				t.Fatal("incomplete or mixed-version source accepted")
			}
		})
	}
}

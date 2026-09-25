package helps

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func ticketProbeTestAuth(id, email, account string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{
		"access_token": id, "email": email, "account_id": account,
	}}
}

func newTicketHarvesterTest(t *testing.T, cfg *config.Config, auths []*cliproxyauth.Auth, rt codexRoundTripFunc) (*CodexTurnStateTicketHarvester, *cliproxyauth.Manager, *atomic.Pointer[config.Config]) {
	t.Helper()
	manager := cliproxyauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		t.Cleanup(func() { InvalidateCodexCookieJar(auth.ID) })
	}
	current := &atomic.Pointer[config.Config]{}
	current.Store(cfg)
	h := NewCodexTurnStateTicketHarvester(CodexTurnStateTicketHarvesterOptions{
		Config: current.Load, List: manager.List,
		Update: func(ctx context.Context, base, updated *cliproxyauth.Auth) error {
			_, errUpdate := manager.UpdatePreparedAuth(ctx, base, updated)
			return errUpdate
		},
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
	h.Start(ctx)
	t.Cleanup(h.Stop)
	return h, manager, current
}

func ticketProbeTestRequest(t *testing.T, req *http.Request) string {
	t.Helper()
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ") + "/" + gjson.GetBytes(body, "model").String()
}

func ticketProbeTestResponse(req *http.Request, status, length int) *http.Response {
	headers := make(http.Header)
	if length > 0 {
		headers.Set(CodexTurnStateTicketHeader, testTicketState(length))
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("")), Request: req}
}

type ticketProbeTestCalls struct {
	mu     sync.Mutex
	values []string
}

func (c *ticketProbeTestCalls) record(value string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, value)
	return len(c.values)
}

func (c *ticketProbeTestCalls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.values...)
}

func TestCodexTicketHarvesterRemainsEnabledWithBasispoints(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{
			Basispoints: config.CodexBasispointsConfig{Enabled: true},
			TurnStateTicket: config.CodexTurnStateTicketConfig{
				Enabled: true, Models: []string{"gpt-6-astra"}, ProbeIntervalSeconds: 3600,
			},
		}}
		calls := 0
		h, manager, current := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("basispoints-ticket", "test@example.com", "test"),
		}, codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return ticketProbeTestResponse(req, http.StatusOK, 780), nil
		}))
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("Basispoints suppressed the configured ticket probe: calls=%d", calls)
		}
		for _, enabled := range []bool{false, true} {
			next := *cfg
			next.Codex.Basispoints.Enabled = enabled
			current.Store(&next)
			h.ConfigChanged()
			synctest.Wait()
			ticket := codexTurnStateTicketForAuth(manager.List()[0], "gpt-6-astra")
			if ticket == nil || !ticket.valid(time.Now()) || calls != 1 {
				t.Fatal("changing Basispoints invalidated the independently harvested ticket")
			}
		}
		h.Stop()
	})
}

func TestCodexTicketHarvesterStartsImmediatelyAndSerializesProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, ProbeIntervalSeconds: 3600}}}
		release := make(chan struct{})
		var calls []string
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls = append(calls, ticketProbeTestRequest(t, req))
			select {
			case <-release:
				return ticketProbeTestResponse(req, 200, 780), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		started := time.Now()
		h, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("a-mac", "a@example.com", "a"),
			ticketProbeTestAuth("b-windows", "a@example.com", "a"),
		}, rt)
		want := []string{"a-mac/gpt-6-astra", "a-mac/gpt-5.6-sol", "b-windows/gpt-6-astra", "b-windows/gpt-5.6-sol"}
		synctest.Wait()
		if !reflect.DeepEqual(calls, want[:1]) || !time.Now().Equal(started) {
			t.Fatalf("startup probes=%v elapsed=%s", calls, time.Since(started))
		}
		for range 3 {
			h.ConfigChanged()
		}
		synctest.Wait()
		if len(calls) != 1 {
			t.Fatalf("config reload bypassed the busy worker: %v", calls)
		}
		for i := range want {
			if !reflect.DeepEqual(calls, want[:i+1]) {
				t.Fatalf("probes were not sequential: %v", calls)
			}
			release <- struct{}{}
			synctest.Wait()
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("config reload repeated a completed probe: %v", calls)
		}
		for _, auth := range manager.List() {
			for _, status := range CodexTurnStateTicketStatuses(auth, cfg.Codex.EffectiveTurnStateTicket(), time.Now()) {
				if !status.Ready {
					t.Errorf("credential %s model %s lost its ticket", auth.ID, status.Model)
				}
			}
		}
	})
}

func TestCodexTicketHarvester312DoesNotPauseOtherModelsOrCredentials(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}
				var calls ticketProbeTestCalls
				rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if calls.record(ticketProbeTestRequest(t, req)) == 1 {
						return ticketProbeTestResponse(req, status, 312), nil
					}
					return ticketProbeTestResponse(req, 200, 780), nil
				})
				auths := []*cliproxyauth.Auth{
					ticketProbeTestAuth("a-mac", "same@example.com", "account-1"),
					ticketProbeTestAuth("b-windows", "same@example.com", "account-1"),
				}
				h, manager, _ := newTicketHarvesterTest(t, cfg, auths, rt)
				SetCodexTurnStateTicketRecorder(h.Record)
				synctest.Wait()
				want := []string{"a-mac/gpt-6-astra", "a-mac/gpt-5.6-sol", "b-windows/gpt-6-astra", "b-windows/gpt-5.6-sol"}
				if !reflect.DeepEqual(calls.snapshot(), want) {
					t.Fatalf("response paused the sweep: %v", calls.snapshot())
				}
				// A normal 312 response and repeated config reloads must not wake a probe.
				auth, _ := manager.GetByID("a-mac")
				RecordCodexTurnStateTicketOnResponse(auth, cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", ticketProbeTestResponse(nil, 200, 312))
				h.ConfigChanged()
				synctest.Wait()
				time.Sleep(59 * time.Second) // synctest virtual time.
				synctest.Wait()
				if !reflect.DeepEqual(calls.snapshot(), want) {
					t.Fatalf("response triggered an immediate probe: %v", calls.snapshot())
				}
				time.Sleep(time.Second)
				synctest.Wait()
				want = append(want, "a-mac/gpt-6-astra")
				if !reflect.DeepEqual(calls.snapshot(), want) {
					t.Fatalf("scheduled sweep did not only fill the missing ticket: %v", calls.snapshot())
				}
				for _, auth := range manager.List() {
					for _, model := range cfg.Codex.EffectiveTurnStateTicket().Models {
						ticket := codexTurnStateTicketForAuth(auth, model)
						if !ticket.valid(time.Now()) || ticket.AccountID != auth.ID {
							t.Fatalf("ticket not independently stored for %s/%s", auth.ID, model)
						}
					}
				}
			})
		})
	}
}

func TestCodexTicketHarvesterRefreshFollowsLatestResponseExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}
		var calls ticketProbeTestCalls
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.record(ticketProbeTestRequest(t, req))
			return ticketProbeTestResponse(req, 200, 780), nil
		})
		h, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{ticketProbeTestAuth("mac", "same@example.com", "account")}, rt)
		SetCodexTurnStateTicketRecorder(h.Record)
		synctest.Wait()
		started := time.Now()
		if len(calls.snapshot()) != 2 {
			t.Fatalf("startup probes = %v", calls.snapshot())
		}
		time.Sleep(20 * time.Minute) // synctest virtual time.
		synctest.Wait()
		if len(calls.snapshot()) != 2 {
			t.Fatal("fresh tickets were probed")
		}
		auth, _ := manager.GetByID("mac")
		resp := ticketProbeTestResponse(nil, 200, 780)
		resp.Header.Set(CodexTurnStateTicketHeader, strings.Repeat("n", 780))
		RecordCodexTurnStateTicketOnResponse(auth, cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", resp)
		h.ConfigChanged() // Simulate the auth-save reload callback.
		synctest.Wait()
		if len(calls.snapshot()) != 2 {
			t.Fatal("new response ticket immediately triggered a probe")
		}
		time.Sleep(29 * time.Minute)
		synctest.Wait()
		if len(calls.snapshot()) != 2 {
			t.Fatal("probed before the ten-minute refresh window")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(calls.snapshot()) != 3 || calls.snapshot()[2] != "mac/gpt-5.6-sol" {
			t.Fatalf("original expiry refresh = %v", calls.snapshot())
		}
		auth, _ = manager.GetByID("mac")
		ticket := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
		if ticket.State != strings.Repeat("n", 780) || !ticket.ExpiresAt.Equal(started.Add(80*time.Minute)) {
			t.Fatal("normal response replacement was lost")
		}
		time.Sleep(19 * time.Minute)
		synctest.Wait()
		if len(calls.snapshot()) != 3 {
			t.Fatal("replacement ticket refreshed too early")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(calls.snapshot()) != 4 || calls.snapshot()[3] != "mac/gpt-6-astra" {
			t.Fatalf("replacement expiry refresh = %v", calls.snapshot())
		}
	})
}

func TestCodexTicketHarvesterFailedRefreshRetainsUnexpiredTicket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}}}
		auth := ticketProbeTestAuth("mac", "same@example.com", "account")
		expiry := time.Now().Add(10 * time.Minute)
		original := strings.Repeat("a", 780)
		StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: original, ExpiresAt: expiry})
		var calls int
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return ticketProbeTestResponse(req, 200, 312), nil
		})
		_, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{auth}, rt)
		synctest.Wait()
		current, _ := manager.GetByID("mac")
		ticket := codexTurnStateTicketForAuth(current, "gpt-6-astra")
		if calls != 1 || ticket.State != original || !ticket.ExpiresAt.Equal(expiry) || !ticket.valid(time.Now()) {
			t.Fatal("failed refresh changed an unexpired ticket")
		}
		headers := make(http.Header)
		if errApply := ApplyCodexTurnStateTicket(current, cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != original {
			t.Fatal("failed refresh stopped retention")
		}
	})
}

func TestCodexTicketHarvester312PreservesEarlierSuccessAndAnonymousIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}
		var calls []string
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls = append(calls, ticketProbeTestRequest(t, req))
			if len(calls) == 2 {
				return ticketProbeTestResponse(req, 200, 312), nil
			}
			return ticketProbeTestResponse(req, 200, 780), nil
		})
		_, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("a-mac", "same@example.com", "account"),
			ticketProbeTestAuth("b-windows", "same@example.com", "account"),
			ticketProbeTestAuth("c-no-identity", "", ""),
			ticketProbeTestAuth("d-no-identity", "", ""),
		}, rt)
		synctest.Wait()
		want := []string{"a-mac/gpt-6-astra", "a-mac/gpt-5.6-sol", "b-windows/gpt-6-astra", "b-windows/gpt-5.6-sol", "c-no-identity/gpt-6-astra", "c-no-identity/gpt-5.6-sol", "d-no-identity/gpt-6-astra", "d-no-identity/gpt-5.6-sol"}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("probe order=%v, want %v", calls, want)
		}
		auth, _ := manager.GetByID("a-mac")
		if !codexTurnStateTicketForAuth(auth, "gpt-6-astra").valid(time.Now()) {
			t.Fatal("312 on the second model deleted the first model's valid ticket")
		}
		if codexTurnStateTicketForAuth(auth, "gpt-5.6-sol") != nil {
			t.Fatal("312 was persisted as a ticket")
		}
	})
}

func TestCodexTicketHarvesterDisableStopsQueueAndStopCancelsProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}
		release := make(chan struct{})
		calls, canceled := 0, false
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			select {
			case <-release:
				return ticketProbeTestResponse(req, 200, 780), nil
			case <-req.Context().Done():
				canceled = true
				return nil, req.Context().Err()
			}
		})
		h, _, current := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{ticketProbeTestAuth("a", "", "")}, rt)
		synctest.Wait()
		disabled := *cfg
		disabled.Codex.TurnStateTicket.Enabled = false
		current.Store(&disabled)
		h.ConfigChanged()
		release <- struct{}{}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("disabled worker continued the queue: %d calls", calls)
		}
		current.Store(cfg)
		h.ConfigChanged()
		synctest.Wait()
		if calls != 2 {
			t.Fatalf("re-enable did not immediately resume missing model: %d calls", calls)
		}
		h.Stop()
		if !canceled {
			t.Fatal("Stop returned without canceling the active probe")
		}
	})
}

func TestCodexTicketCacheAllModelsNeverExpandsProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, FailClosed: true}}}
		var calls atomic.Int32
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			key := ticketProbeTestRequest(t, req)
			if !strings.HasSuffix(key, "/gpt-6-astra") && !strings.HasSuffix(key, "/gpt-5.6-sol") {
				t.Errorf("normal-response model was proactively probed: %s", key)
			}
			calls.Add(1)
			return ticketProbeTestResponse(req, 200, 780), nil
		})
		h, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("mac", "same@example.com", "account"),
			ticketProbeTestAuth("windows", "same@example.com", "account"),
		}, rt)
		SetCodexTurnStateTicketRecorder(h.Record)
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatalf("startup probes = %d, want exactly 4", calls.Load())
		}
		policy := cfg.Codex.EffectiveTurnStateTicket()
		for _, auth := range manager.List() {
			for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-terra"} {
				RecordCodexTurnStateTicketOnResponse(auth, policy, model, ticketProbeTestResponse(nil, 200, 780))
			}
		}
		mac, _ := manager.GetByID("mac")
		RecordCodexTurnStateTicketOnResponse(mac, policy, "gpt-5.6-luna", ticketProbeTestResponse(nil, 200, 312))
		synctest.Wait()
		current, _ := manager.GetByID("mac")
		if !codexTurnStateTicketForAuth(current, "gpt-5.6-luna").valid(time.Now()) || !codexTurnStateTicketForAuth(mac, "gpt-5.6-luna").valid(time.Now()) {
			t.Fatal("312 changed retained Luna tickets")
		}
		windows, _ := manager.GetByID("windows")
		if !codexTurnStateTicketForAuth(windows, "gpt-5.6-luna").valid(time.Now()) {
			t.Fatal("Mac response affected Windows ticket")
		}
		time.Sleep(5 * time.Minute) // synctest virtual time.
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatalf("normal response added probes: %d", calls.Load())
		}
		time.Sleep(55 * time.Minute) // Cross both the refresh window and ticket expiry.
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("only Astra/Sol should refresh: got %d total probes", calls.Load())
		}
		current, _ = manager.GetByID("mac")
		if codexTurnStateTicketForAuth(current, "gpt-5.6-terra").valid(time.Now()) {
			t.Fatal("expired normal-response ticket was renewed by the harvester")
		}
		headers := http.Header{CodexTurnStateTicketHeader: {"passive-state"}}
		if errApply := ApplyCodexTurnStateTicket(current, policy, "gpt-5.6-terra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != "passive-state" {
			t.Fatal("expired extra model must fall back without fail-closed blocking")
		}
	})
}

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

func TestCodexTicketHarvesterStartsImmediatelyAndSerializesInvalidations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, ProbeIntervalSeconds: 3600}}}
		release := make(chan struct{})
		var calls []string
		rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls = append(calls, ticketProbeTestRequest(t, req))
			select {
			case <-release:
				return ticketProbeTestResponse(req, 200, 292), nil
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
			h.Invalidate("b-windows", "gpt-5.6-sol")
		}
		synctest.Wait()
		if len(calls) != 1 {
			t.Fatalf("invalidation bypassed the busy worker: %v", calls)
		}
		for i := range want {
			if !reflect.DeepEqual(calls, want[:i+1]) {
				t.Fatalf("probes were not sequential: %v", calls)
			}
			release <- struct{}{}
			synctest.Wait()
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("queued invalidation repeated a completed probe: %v", calls)
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

func TestCodexTicketHarvester312PausesAccountAliasesUntilNextInterval(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, ProbeIntervalSeconds: 60}}}
				var callsMu sync.Mutex
				var recordedCalls []string
				snapshot := func() []string {
					callsMu.Lock()
					defer callsMu.Unlock()
					return append([]string(nil), recordedCalls...)
				}
				rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					callsMu.Lock()
					recordedCalls = append(recordedCalls, ticketProbeTestRequest(t, req))
					count := len(recordedCalls)
					callsMu.Unlock()
					if count == 1 {
						return ticketProbeTestResponse(req, status, 312), nil
					}
					return ticketProbeTestResponse(req, 200, 292), nil
				})
				auths := []*cliproxyauth.Auth{
					ticketProbeTestAuth("a-mac", " Alice@Example.com ", "account-1"),
					ticketProbeTestAuth("b-windows", "another@example.com", "account-1"),
					ticketProbeTestAuth("c-email", "alice@example.com", "account-2"),
					ticketProbeTestAuth("d-account-only", "", "account-2"),
					ticketProbeTestAuth("e-unrelated", "bob@example.com", "account-3"),
				}
				h, manager, current := newTicketHarvesterTest(t, cfg, auths, rt)
				synctest.Wait()
				wantFirst := []string{"a-mac/gpt-6-astra", "e-unrelated/gpt-6-astra", "e-unrelated/gpt-5.6-sol"}
				if calls := snapshot(); !reflect.DeepEqual(calls, wantFirst) {
					t.Fatalf("312 did not pause all related credentials/models: %v", calls)
				}
				h.Invalidate("d-account-only", "gpt-5.6-sol")
				synctest.Wait()
				disabled := *cfg
				disabled.Codex.TurnStateTicket.Enabled = false
				current.Store(&disabled)
				h.ConfigChanged()
				current.Store(cfg)
				h.ConfigChanged()
				synctest.Wait()
				if calls := snapshot(); !reflect.DeepEqual(calls, wantFirst) {
					t.Fatalf("invalidation or re-enabling bypassed account pause: %v", calls)
				}
				// synctest advances virtual time; no wall-clock wait is used.
				time.Sleep(59 * time.Second)
				synctest.Wait()
				if calls := snapshot(); len(calls) != 3 {
					t.Fatalf("account retried too early: %v", calls)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				want := append([]string{}, wantFirst...)
				for _, auth := range auths[:4] {
					want = append(want, auth.ID+"/gpt-6-astra", auth.ID+"/gpt-5.6-sol")
				}
				if calls := snapshot(); !reflect.DeepEqual(calls, want) {
					t.Fatalf("next interval probes=%v, want %v", calls, want)
				}
				for _, auth := range manager.List() {
					for _, model := range cfg.Codex.EffectiveTurnStateTicket().Models {
						ticket := codexTurnStateTicketForAuth(auth, model)
						if !ticket.valid(time.Now(), 292) || ticket.AccountID != auth.ID {
							t.Errorf("ticket was not saved independently for %s/%s", auth.ID, model)
						}
					}
				}
			})
		})
	}
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
			return ticketProbeTestResponse(req, 200, 292), nil
		})
		_, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("a-mac", "same@example.com", "account"),
			ticketProbeTestAuth("b-windows", "same@example.com", "account"),
			ticketProbeTestAuth("c-no-identity", "", ""),
			ticketProbeTestAuth("d-no-identity", "", ""),
		}, rt)
		synctest.Wait()
		want := []string{"a-mac/gpt-6-astra", "a-mac/gpt-5.6-sol", "c-no-identity/gpt-6-astra", "c-no-identity/gpt-5.6-sol", "d-no-identity/gpt-6-astra", "d-no-identity/gpt-5.6-sol"}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("probe order=%v, want %v", calls, want)
		}
		auth, _ := manager.GetByID("a-mac")
		if !codexTurnStateTicketForAuth(auth, "gpt-6-astra").valid(time.Now(), 292) {
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
				return ticketProbeTestResponse(req, 200, 292), nil
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
			return ticketProbeTestResponse(req, 200, 292), nil
		})
		h, manager, _ := newTicketHarvesterTest(t, cfg, []*cliproxyauth.Auth{
			ticketProbeTestAuth("mac", "same@example.com", "account"),
			ticketProbeTestAuth("windows", "same@example.com", "account"),
		}, rt)
		SetCodexTurnStateTicketRecorder(h.Record)
		SetCodexTurnStateTicketInvalidator(h.Invalidate)
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatalf("startup probes = %d, want exactly 4", calls.Load())
		}
		policy := cfg.Codex.EffectiveTurnStateTicket()
		for _, auth := range manager.List() {
			for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-terra"} {
				InvalidateCodexTurnStateTicketOnResponse(auth, policy, model, ticketProbeTestResponse(nil, 200, 292))
			}
		}
		mac, _ := manager.GetByID("mac")
		InvalidateCodexTurnStateTicketOnResponse(mac, policy, "gpt-5.6-luna", ticketProbeTestResponse(nil, 200, 312))
		synctest.Wait()
		current, _ := manager.GetByID("mac")
		if codexTurnStateTicketForAuth(current, "gpt-5.6-luna") != nil || codexTurnStateTicketForAuth(mac, "gpt-5.6-luna") != nil {
			t.Fatal("312 did not invalidate persisted and request-local Luna tickets")
		}
		windows, _ := manager.GetByID("windows")
		if !codexTurnStateTicketForAuth(windows, "gpt-5.6-luna").valid(time.Now(), 292) {
			t.Fatal("Mac invalidation affected Windows ticket")
		}
		time.Sleep(5 * time.Minute) // synctest virtual time.
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatalf("normal response or invalidation added probes: %d", calls.Load())
		}
		time.Sleep(55 * time.Minute) // Cross both the refresh window and ticket expiry.
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("only Astra/Sol should refresh: got %d total probes", calls.Load())
		}
		current, _ = manager.GetByID("mac")
		if codexTurnStateTicketForAuth(current, "gpt-5.6-terra").valid(time.Now(), 292) {
			t.Fatal("expired normal-response ticket was renewed by the harvester")
		}
		headers := http.Header{CodexTurnStateTicketHeader: {"passive-state"}}
		if errApply := ApplyCodexTurnStateTicket(current, policy, "gpt-5.6-terra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != "passive-state" {
			t.Fatal("expired extra model must fall back without fail-closed blocking")
		}
	})
}

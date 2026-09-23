package cliproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type ticketTestStore struct {
	*sdkauth.FileTokenStore
	saved chan struct{}
}

func (s *ticketTestStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, errSave := s.FileTokenStore.Save(ctx, auth)
	if errSave == nil {
		s.saved <- struct{}{}
	}
	return path, errSave
}

type ticketTestTransport func(*http.Request) (*http.Response, error)

func (f ticketTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func ticketTestWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ticket test synchronization")
		var zero T
		return zero
	}
}

func newTicketTestService(t *testing.T, expired bool) (*Service, *ticketTestStore, *coreauth.Auth) {
	t.Helper()
	store := &ticketTestStore{FileTokenStore: sdkauth.NewFileTokenStore(), saved: make(chan struct{}, 16)}
	dir := t.TempDir()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID: "codex-ticket-test-pro.json", FileName: "codex-ticket-test-pro.json", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, coreauth.AttributePath: filepath.Join(dir, "codex-ticket-test-pro.json")},
		Metadata:   map[string]any{"type": "codex", "access_token": "test-token", "plan_type": "pro", "note": "keep"},
	}
	if expired {
		for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
			helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: model, State: "gAAAAA" + strings.Repeat("e", 774), CapturedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)})
		}
	}
	registered, errRegister := manager.Register(t.Context(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	ticketTestWait(t, store.saved)
	service := &Service{coreManager: manager, cfg: &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, FailClosed: true, ProbeIntervalSeconds: 3600}}}}
	return service, store, registered
}

func assertTicketModelsReady(t *testing.T, service *Service, auth *coreauth.Auth) {
	t.Helper()
	statuses := helps.CodexTurnStateTicketStatuses(auth, service.cfg.Codex.EffectiveTurnStateTicket(), time.Now())
	if len(statuses) != 2 {
		t.Fatalf("ticket statuses = %+v", statuses)
	}
	for _, status := range statuses {
		if !status.Ready || status.Blocked || status.Length != 780 || status.RemainingSeconds < 3500 {
			t.Errorf("model %s lost its new ticket: %+v", status.Model, status)
		}
	}
}

func TestCodexTicketSequentialProbesPreserveBothModels(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, first := range []string{"gpt-5.6-sol", "gpt-6-astra"} {
			name := "missing/" + first
			if expired {
				name = "expired/" + first
			}
			t.Run(name, func(t *testing.T) {
				service, store, auth := newTicketTestService(t, expired)
				second := "gpt-6-astra"
				if first == second {
					second = "gpt-5.6-sol"
				}
				service.cfg.Codex.TurnStateTicket.Models = []string{first, second}
				releases := map[string]chan struct{}{"gpt-6-astra": make(chan struct{}), "gpt-5.6-sol": make(chan struct{})}
				started := make(chan string, 2)
				rt := ticketTestTransport(func(req *http.Request) (*http.Response, error) {
					body, errRead := io.ReadAll(req.Body)
					if errRead != nil {
						return nil, errRead
					}
					model := gjson.GetBytes(body, "model").String()
					started <- model
					select {
					case <-releases[model]:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {"gAAAAA" + strings.Repeat("n", 774)}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
				service.startCodexTicketHarvester(ctx)
				defer service.stopCodexTicketHarvester()
				if got := ticketTestWait(t, started); got != first {
					t.Fatalf("first probe = %s, want %s", got, first)
				}
				close(releases[first])
				ticketTestWait(t, store.saved)
				if got := ticketTestWait(t, started); got != second {
					t.Fatalf("second probe = %s, want %s", got, second)
				}
				close(releases[second])
				ticketTestWait(t, store.saved)
				current, _ := service.coreManager.GetByID(auth.ID)
				assertTicketModelsReady(t, service, current)
				path := current.Attributes[coreauth.AttributePath]
				data, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				parsed, errParse := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: filepath.Dir(path), Config: service.cfg, Now: time.Now()}, path, data)
				if errParse != nil || len(parsed) != 1 {
					t.Fatalf("reload parse: %v", errParse)
				}
				assertTicketModelsReady(t, service, parsed[0])
				if _, errUpdate := service.coreManager.Update(coreauth.WithSkipPersist(t.Context()), parsed[0]); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				reloaded, _ := service.coreManager.GetByID(auth.ID)
				assertTicketModelsReady(t, service, reloaded)
				handler := management.NewHandler(service.cfg, "", service.coreManager)
				for _, endpoint := range []struct {
					handle gin.HandlerFunc
					path   string
				}{{handler.GetCodexTurnStateTicket, "accounts.0.tickets"}, {handler.ListAuthFiles, "files.0.codex_turn_tickets"}} {
					recorder := httptest.NewRecorder()
					ginCtx, _ := gin.CreateTestContext(recorder)
					ginCtx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
					endpoint.handle(ginCtx)
					statuses := gjson.GetBytes(recorder.Body.Bytes(), endpoint.path).Array()
					if recorder.Code != http.StatusOK || len(statuses) != 2 {
						t.Fatalf("management ticket response status=%d count=%d", recorder.Code, len(statuses))
					}
					for _, status := range statuses {
						if !status.Get("ready").Bool() || status.Get("blocked").Bool() || status.Get("remaining_seconds").Int() < 3500 {
							t.Errorf("management API returned an unready model: %s", status.Raw)
						}
					}
				}
			})
		}
	}
}

func TestCodexTicketConfigCommitImmediatelyStartsEnabledProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := coreauth.NewManager(nil, nil, nil)
		auth := &coreauth.Auth{ID: "config-wakeup", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{ProbeIntervalSeconds: 3600, Models: []string{"gpt-6-astra"}}}}
		service := &Service{coreManager: manager, cfg: cfg}
		calls := 0
		rt := ticketTestTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		})
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
		service.startCodexTicketHarvester(ctx)
		defer service.stopCodexTicketHarvester()
		synctest.Wait()
		if calls != 0 {
			t.Fatal("disabled service sent a probe")
		}
		started := time.Now()
		updated := *cfg
		updated.Codex.TurnStateTicket.Enabled = true
		service.commitConfigUpdate(&updated)
		synctest.Wait()
		if calls != 1 || !time.Now().Equal(started) {
			t.Fatalf("enable did not probe immediately: calls=%d elapsed=%s", calls, time.Since(started))
		}
		service.commitConfigUpdate(&updated)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("unchanged config caused an extra probe")
		}
	})
}

func TestCodexTicketNormalResponsesPreserveOtherModelsAndCredentials(t *testing.T) {
	service, _, auth := newTicketTestService(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.startCodexTicketHarvester(ctx)
	defer service.stopCodexTicketHarvester()
	first, _ := service.coreManager.GetByID(auth.ID)
	second, _ := service.coreManager.GetByID(auth.ID)
	updated := auth.Clone()
	updated.Metadata["access_token"] = "refreshed-token"
	updated.Metadata["note"] = "updated-note"
	if _, errUpdate := service.coreManager.Update(t.Context(), updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	policy := service.cfg.Codex.EffectiveTurnStateTicket()
	resp := &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {"gAAAAA" + strings.Repeat("r", 774)}}}
	helps.RecordCodexTurnStateTicketOnResponse(first, policy, "gpt-5.6-sol", resp)
	helps.RecordCodexTurnStateTicketOnResponse(second, policy, "gpt-6-astra", resp)
	current, _ := service.coreManager.GetByID(auth.ID)
	assertTicketModelsReady(t, service, current)
	if current.Metadata["access_token"] != "refreshed-token" || current.Metadata["note"] != "updated-note" {
		t.Error("ticket update reverted concurrent credential or note changes")
	}
	// Response-local auth values must still receive the observed ticket.
	for _, pair := range []struct {
		auth  *coreauth.Auth
		model string
	}{{first, "gpt-5.6-sol"}, {second, "gpt-6-astra"}} {
		raw, errMarshal := json.Marshal(pair.auth.Metadata[helps.CodexTurnStateTicketMetadataKey(pair.model)])
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if gjson.GetBytes(raw, "state").String() != resp.Header.Get(helps.CodexTurnStateTicketHeader) {
			t.Errorf("request-local ticket missing for %s", pair.model)
		}
	}
}

func TestCodexTicketAllModelsPersistsReloadsAndReportsStatus(t *testing.T) {
	service, store, auth := newTicketTestService(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.startCodexTicketHarvester(ctx)
	defer service.stopCodexTicketHarvester()
	policy := service.cfg.Codex.EffectiveTurnStateTicket()
	states := map[string]string{
		"gpt-5.6-luna":  "gAAAAA" + strings.Repeat("l", 774),
		"gpt-5.6-terra": "gAAAAA" + strings.Repeat("t", 774),
	}
	for model, state := range states {
		// Use the same original snapshot to also exercise per-model merge persistence.
		helps.RecordCodexTurnStateTicketOnResponse(auth.Clone(), policy, model, &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {state}}})
		ticketTestWait(t, store.saved)
	}
	// A later response must replace the same model on disk without losing siblings.
	states["gpt-5.6-luna"] = strings.Repeat("n", 780)
	current, _ := service.coreManager.GetByID(auth.ID)
	helps.RecordCodexTurnStateTicketOnResponse(current, policy, "gpt-5.6-luna", &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {states["gpt-5.6-luna"]}}})
	ticketTestWait(t, store.saved)
	helps.RecordCodexTurnStateTicketOnResponse(current, policy, "gpt-5.6-luna", &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {strings.Repeat("x", 312)}}})
	path := auth.Attributes[coreauth.AttributePath]
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	parsed, errParse := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: filepath.Dir(path), Config: service.cfg, Now: time.Now()}, path, data)
	if errParse != nil || len(parsed) != 1 {
		t.Fatalf("reload parse: %v", errParse)
	}
	for model, state := range states {
		headers := make(http.Header)
		if errApply := helps.ApplyCodexTurnStateTicket(parsed[0], policy, model, headers); errApply != nil || headers.Get(helps.CodexTurnStateTicketHeader) != state {
			t.Fatalf("reloaded %s ticket was not independently reused: %v", model, errApply)
		}
	}
	handler := management.NewHandler(service.cfg, "", service.coreManager)
	for _, endpoint := range []struct {
		handle gin.HandlerFunc
		path   string
	}{{handler.GetCodexTurnStateTicket, "accounts.0.tickets"}, {handler.ListAuthFiles, "files.0.codex_turn_tickets"}} {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		endpoint.handle(ginCtx)
		statuses := gjson.GetBytes(recorder.Body.Bytes(), endpoint.path).Array()
		if recorder.Code != http.StatusOK || len(statuses) != 4 {
			t.Fatalf("management returned %d statuses, want 4", len(statuses))
		}
		for _, status := range statuses {
			if _, learned := states[status.Get("model").String()]; learned && (!status.Get("ready").Bool() || status.Get("blocked").Bool() || status.Get("remaining_seconds").Int() < 3500) {
				t.Fatalf("normal-response status = %s", status.Raw)
			}
		}
		for _, state := range states {
			if strings.Contains(recorder.Body.String(), state) {
				t.Fatal("management response exposed ticket contents")
			}
		}
	}
}

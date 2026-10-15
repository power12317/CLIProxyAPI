package cliproxy

import (
	"bytes"
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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
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
	registered, errRegister := manager.Register(t.Context(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if expired {
		for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
			helps.StoreCodexTurnStateTicket(registered, helps.CodexTurnStateTicket{Model: model, State: "gAAAAA" + strings.Repeat("e", 774), CapturedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)})
		}
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
				handler := management.NewHandler(nil, "", nil)
				synctest.Test(t, func(t *testing.T) {
					service, store, auth := newTicketTestService(t, expired)
					original, errRead := os.ReadFile(auth.Attributes[coreauth.AttributePath])
					if errRead != nil {
						t.Fatal(errRead)
					}
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
					synctest.Wait()
					if got := ticketTestWait(t, started); got != second {
						t.Fatalf("second probe = %s, want %s", got, second)
					}
					close(releases[second])
					synctest.Wait()
					current, _ := service.coreManager.GetByID(auth.ID)
					assertTicketModelsReady(t, service, current)
					path := current.Attributes[coreauth.AttributePath]
					data, errRead := os.ReadFile(path)
					if errRead != nil {
						t.Fatal(errRead)
					}
					if !bytes.Equal(original, data) || len(store.saved) != 0 {
						t.Fatal("probe wrote auth JSON")
					}
					parsed, errParse := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: filepath.Dir(path), Config: service.cfg, Now: time.Now()}, path, data)
					if errParse != nil || len(parsed) != 1 {
						t.Fatalf("reload parse: %v", errParse)
					}
					if _, errUpdate := service.coreManager.Update(coreauth.WithSkipPersist(t.Context()), parsed[0]); errUpdate != nil {
						t.Fatal(errUpdate)
					}
					reloaded, _ := service.coreManager.GetByID(auth.ID)
					assertTicketModelsReady(t, service, reloaded)
					handler.SetConfig(service.cfg)
					handler.SetAuthManager(service.coreManager)
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
					service.stopCodexTicketHarvester()
					for _, status := range helps.CodexTurnStateTicketStatuses(parsed[0], service.cfg.Codex.EffectiveTurnStateTicket(), time.Now()) {
						if status.Ready {
							t.Fatal("cold service recovered a ticket from auth JSON")
						}
					}
				})
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
	// Existing auth snapshots read the shared memory cache without metadata writes.
	for _, pair := range []struct {
		auth  *coreauth.Auth
		model string
	}{{first, "gpt-5.6-sol"}, {second, "gpt-6-astra"}} {
		headers := make(http.Header)
		if err := helps.ApplyCodexTurnStateTicket(pair.auth, policy, pair.model, headers); err != nil || headers.Get(helps.CodexTurnStateTicketHeader) != resp.Header.Get(helps.CodexTurnStateTicketHeader) {
			t.Fatalf("memory ticket missing for %s: %v", pair.model, err)
		}
		for key := range pair.auth.Metadata {
			if helps.IsCodexTurnStateTicketMetadataKey(key) {
				t.Fatal("ticket added to credential metadata")
			}
		}
	}
}

func TestCodexTicketAllModelsMemorySurvivesReloadAndReportsStatus(t *testing.T) {
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
		// Use the same original snapshot to exercise independent model entries.
		helps.RecordCodexTurnStateTicketOnResponse(auth.Clone(), policy, model, &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {state}}})
	}
	// A later response replaces only this model in memory.
	states["gpt-5.6-luna"] = strings.Repeat("n", 780)
	current, _ := service.coreManager.GetByID(auth.ID)
	helps.RecordCodexTurnStateTicketOnResponse(current, policy, "gpt-5.6-luna", &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {states["gpt-5.6-luna"]}}})
	helps.RecordCodexTurnStateTicketOnResponse(current, policy, "gpt-5.6-luna", &http.Response{StatusCode: 200, Header: http.Header{helps.CodexTurnStateTicketHeader: {strings.Repeat("x", 312)}}})
	path := auth.Attributes[coreauth.AttributePath]
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if len(store.saved) != 0 || bytes.Contains(data, []byte("codex_turn_ticket:")) {
		t.Fatal("normal response wrote auth JSON")
	}
	parsed, errParse := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{AuthDir: filepath.Dir(path), Config: service.cfg, Now: time.Now()}, path, data)
	if errParse != nil || len(parsed) != 1 {
		t.Fatalf("reload parse: %v", errParse)
	}
	reloaded, errUpdate := service.coreManager.Update(coreauth.WithSkipPersist(t.Context()), parsed[0])
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for model, state := range states {
		headers := make(http.Header)
		if errApply := helps.ApplyCodexTurnStateTicket(reloaded, policy, model, headers); errApply != nil || headers.Get(helps.CodexTurnStateTicketHeader) != state {
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

func TestCodexTicketMemorySurvivesGenericResultPersistence(t *testing.T) {
	service, store, auth := newTicketTestService(t, false)
	t.Cleanup(func() { helps.InvalidateCodexTurnStateTickets(auth.ID) })
	state := strings.Repeat("memory-ticket", 60)
	state += strings.Repeat("m", 780-len(state))
	helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: "gpt-6-astra", State: state, CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if len(store.saved) != 0 {
		t.Fatal("ticket caching called the credential store")
	}
	path := auth.Attributes[coreauth.AttributePath]
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["codex_turn_ticket:legacy"] = map[string]any{"state": "old-file-ticket"}
	legacy, _ := json.Marshal(metadata)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	for _, success := range []bool{true, false} {
		result := coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-6-astra", Success: success, RegistrationEpoch: auth.RegistrationEpoch, CredentialVersion: auth.CredentialVersion}
		if !success {
			result.Error = &coreauth.Error{Code: coreauth.ErrorCodeRequestScoped, HTTPStatus: 503, Message: "codex ticket borrowing unavailable"}
		}
		service.coreManager.MarkResult(t.Context(), result)
		if len(store.saved) != 1 {
			t.Fatal("generic result path did not call the existing credential save")
		}
		<-store.saved
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(saved, []byte("codex_turn_ticket:")) || bytes.Contains(saved, []byte(state)) || bytes.Contains(saved, []byte("old-file-ticket")) {
			t.Fatal("generic result persistence wrote a ticket")
		}
		if !success && !bytes.Equal(saved, raw) {
			t.Fatal("request-scoped failure changed unchanged credential JSON")
		}
		raw = saved
		current, _ := service.coreManager.GetByID(auth.ID)
		headers := make(http.Header)
		if err := helps.ApplyCodexTurnStateTicket(current, service.cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", headers); err != nil || headers.Get(helps.CodexTurnStateTicketHeader) != state {
			t.Fatalf("generic persistence lost the memory ticket: %v", err)
		}
		if !success && current.Unavailable {
			t.Fatal("request-scoped borrowing error disabled the credential")
		}
	}
}

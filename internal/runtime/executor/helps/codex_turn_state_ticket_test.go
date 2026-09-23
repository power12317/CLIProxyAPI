package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func testTicketState(length int) string {
	return strings.Repeat("t", length)
}

func TestCodexTicketCacheAllModelsCaptureAndOverrides(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name, model, plan string
		enabled, capture  bool
		cacheAll          *bool
		length            int
	}{
		{name: "luna default", model: "gpt-5.6-luna", enabled: true, capture: true, length: 780},
		{name: "terra default", model: "gpt-5.6-terra", enabled: true, capture: true, length: 780},
		{name: "team", model: "gpt-5.6-luna", plan: "team", enabled: true, capture: true, length: 780},
		{name: "business", model: "gpt-5.6-luna", plan: "business", enabled: true, capture: true, length: 780},
		{name: "312 never captured", model: "gpt-5.6-luna", enabled: true, length: 312},
		{name: "sub switch off", model: "gpt-5.6-luna", enabled: true, cacheAll: &disabled, length: 780},
		{name: "master off", model: "gpt-5.6-luna", length: 780},
		{name: "configured still captured", model: "gpt-6-astra", enabled: true, capture: true, cacheAll: &disabled, length: 780},
	} {
		for _, event := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/event=%v", tc.name, event), func(t *testing.T) {
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test", "plan_type": tc.plan}}
				cfg := config.CodexTurnStateTicketConfig{Enabled: tc.enabled, CacheAllModels: tc.cacheAll, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, FailClosed: true}
				value := testTicketState(tc.length)
				if event {
					payload, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{CodexTurnStateTicketHeader: value}})
					RecordCodexTurnStateTicketOnEvent(auth, cfg, tc.model, payload)
				} else {
					RecordCodexTurnStateTicketOnResponse(auth, cfg, tc.model, &http.Response{StatusCode: 200, Header: http.Header{CodexTurnStateTicketHeader: {value}}})
				}
				ticket := codexTurnStateTicketForAuth(auth, tc.model)
				if (ticket != nil) != tc.capture {
					t.Fatalf("captured = %v, want %v", ticket != nil, tc.capture)
				}
				if tc.capture && ticket.ExpiresAt.Sub(ticket.CapturedAt) != time.Hour {
					t.Fatal("normal-response ticket must default to one hour")
				}
				headers := http.Header{CodexTurnStateTicketHeader: {"passive-312"}}
				if errApply := ApplyCodexTurnStateTicket(auth, cfg, tc.model, headers); errApply != nil {
					t.Fatal(errApply)
				}
				body := ApplyCodexTurnStateTicketBody(auth, cfg, tc.model, []byte(`{"client_metadata":{"turn_id":"new-turn","x-codex-turn-state":"passive-312"}}`))
				want := "passive-312"
				if tc.capture {
					want = value
				}
				if headers.Get(CodexTurnStateTicketHeader) != want || gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != want {
					t.Fatal("header/body ticket override did not follow capture policy")
				}
				if tc.capture && tc.model != "gpt-6-astra" {
					cfg.CacheAllModels = &disabled
					headers = http.Header{CodexTurnStateTicketHeader: {"original"}}
					if errApply := ApplyCodexTurnStateTicket(auth, cfg, tc.model, headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != "original" {
						t.Fatal("disabled sub switch still injected an extra model ticket")
					}
				}
			})
		}
	}
}

func TestCodexTicketCacheAllModelsExpiryAndStatus(t *testing.T) {
	now := time.Now()
	auth := &cliproxyauth.Auth{ID: "all-models", Provider: "codex", Metadata: map[string]any{"access_token": "test"}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, FailClosed: true}
	for model, expiry := range map[string]time.Time{"gpt-5.6-luna": now.Add(5 * time.Minute), "gpt-5.6-terra": now.Add(-time.Minute)} {
		StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: model, State: testTicketState(780), ExpiresAt: expiry})
		headers := make(http.Header)
		if errApply := ApplyCodexTurnStateTicket(auth, cfg, model, headers); errApply != nil {
			t.Fatal("extra model must not be blocked by fail-closed", errApply)
		}
		if (headers.Get(CodexTurnStateTicketHeader) != "") != expiry.After(now) {
			t.Fatal("extra ticket expiry differs from the one-hour ticket policy")
		}
	}
	statuses := CodexTurnStateTicketStatuses(auth, cfg, now)
	if len(statuses) != 4 || statuses[2].Model != "gpt-5.6-luna" || !statuses[2].Ready || statuses[2].RemainingSeconds != 300 || statuses[2].Blocked || statuses[3].Model != "gpt-5.6-terra" || statuses[3].Ready || statuses[3].Blocked {
		t.Fatalf("extra model statuses = %+v", statuses)
	}
	disabled := false
	cfg.CacheAllModels = &disabled
	if statuses := CodexTurnStateTicketStatuses(auth, cfg, now); len(statuses) != 2 {
		t.Fatalf("disabled sub switch returned %d statuses", len(statuses))
	}
}

func TestCodexTurnStateTicketConfiguredModelInjection(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: map[string]any{},
	}
	state := testTicketState(config.DefaultCodexTurnStateTicketTargetLength)
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, FailClosed: true}
	headers := make(http.Header)
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil {
		t.Fatalf("ApplyCodexTurnStateTicket() error = %v", errApply)
	}
	if got := headers.Get(CodexTurnStateTicketHeader); got != state {
		t.Fatalf("ticket header = %q", got)
	}
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-5.5", make(http.Header)); errApply != nil {
		t.Fatalf("ungated model was blocked: %v", errApply)
	}
	body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{"client_metadata":{}}`))
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != state {
		t.Fatalf("websocket ticket = %q", got)
	}
}

func TestCodexTurnStateTicketFailClosedAndExpiry(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, FailClosed: true}
	errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", make(http.Header))
	if !errors.Is(errApply, ErrCodexTurnStateTicketUnavailable) {
		t.Fatalf("missing ticket error = %v", errApply)
	}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: testTicketState(780), ExpiresAt: time.Now().Add(-time.Second)})
	if errApply = ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", make(http.Header)); !errors.Is(errApply, ErrCodexTurnStateTicketUnavailable) {
		t.Fatalf("expired ticket error = %v", errApply)
	}
}

func TestCodexTurnStateTicketFailOpenPreservesPassiveStateBeforeSending(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, FailClosed: false}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: strings.Repeat("x", 312), ExpiresAt: time.Now().Add(time.Hour)})
	headers := http.Header{CodexTurnStateTicketHeader: {"passive-state"}}
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil {
		t.Fatalf("fail-open ticket apply error = %v", errApply)
	}
	if got := headers.Get(CodexTurnStateTicketHeader); got != "passive-state" {
		t.Fatalf("fail-open request did not preserve passive header %q", got)
	}
	body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{"client_metadata":{"x-codex-turn-state":"passive-state"}}`))
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != "passive-state" {
		t.Fatalf("fail-open websocket body did not preserve passive state %q", got)
	}
}

func TestCodexTurnStateTicketResponseValueIsStored(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-response", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, TTLSeconds: 3600}
	state := testTicketState(config.DefaultCodexTurnStateTicketTargetLength)
	var recorded string
	SetCodexTurnStateTicketRecorder(func(gotAuth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
		recorded = gotAuth.ID + "/" + ticket.Model
	})
	defer SetCodexTurnStateTicketRecorder(nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {state}}}
	if !RecordCodexTurnStateTicketOnResponse(auth, cfg, "gpt-6-astra", resp) {
		t.Fatal("780 response was not saved")
	}
	ticket := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
	if ticket == nil || !ticket.valid(time.Now()) {
		t.Fatalf("stored response ticket = %#v", ticket)
	}
	if recorded != "auth-response/gpt-6-astra" {
		t.Fatalf("response ticket recorder = %q", recorded)
	}
}

func TestCodexTurnStateTicketStatusShowsInvalidLength(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-invalid", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{}}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: strings.Repeat("x", 312), ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
	if len(statuses) != 1 || statuses[0].Ready || statuses[0].Length != 312 {
		t.Fatalf("invalid ticket status = %#v", statuses)
	}
}

func TestHarvestCodexTurnStateTicketWithoutProxy(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-direct", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{"access_token": "token"}}
	state := testTicketState(config.DefaultCodexTurnStateTicketTargetLength)
	cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, AttemptTimeoutSeconds: 2}}}
	roundTripper := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {state}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripper))
	ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, cfg, auth, "gpt-6-astra", "")
	if errHarvest != nil {
		t.Fatalf("direct harvest error = %v", errHarvest)
	}
	if status != http.StatusOK || ticket.Length != config.DefaultCodexTurnStateTicketTargetLength {
		t.Fatalf("direct harvest status=%d ticket=%#v", status, ticket)
	}
}

func TestHarvestCodexTurnStateTicketMatchesConversationInstallation(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name       string
		system     string
		accountID  string
		wantSystem string
		cfg        *config.Config
		wantFixed  bool
	}{
		{name: "mac enabled", system: "mac", accountID: "shared-account", wantSystem: "mac", cfg: &config.Config{Codex: config.CodexConfig{DeviceConvergence: &enabled}}, wantFixed: true},
		{name: "windows enabled", system: "windows", accountID: "shared-account", wantSystem: "windows", cfg: &config.Config{Codex: config.CodexConfig{DeviceConvergence: &enabled}}, wantFixed: true},
		{name: "legacy default", accountID: "shared-account", wantSystem: "mac", cfg: &config.Config{}, wantFixed: true},
		{name: "nil config", system: "windows", accountID: "shared-account", wantSystem: "windows", wantFixed: true},
		{name: "credential fallback", system: "windows", accountID: "  ", wantSystem: "windows", cfg: &config.Config{}, wantFixed: true},
		{name: "disabled", system: "windows", accountID: "shared-account", wantSystem: "windows", cfg: &config.Config{Codex: config.CodexConfig{DeviceConvergence: &disabled}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID: "probe-identity/" + tc.name, Provider: "codex",
				Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth},
				Metadata: map[string]any{
					"access_token": "test-token", "account_id": tc.accountID, "codex_client_system": tc.system,
				},
			}
			t.Cleanup(func() { InvalidateCodexCookieJar(auth.ID) })
			accountID := strings.TrimSpace(tc.accountID)
			if accountID == "" {
				accountID = auth.ID
			}
			conversation := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{}"}}`)
			_, identity, ok := ApplyCodexOAuthFidelity(conversation, accountID, tc.wantSystem, tc.wantFixed)
			if !ok || (tc.wantFixed && identity.InstallationID == "") {
				t.Fatal("failed to prepare normal conversation identity")
			}
			sessions, turns := make(map[string]bool), make(map[string]bool)
			state := testTicketState(780)
			roundTripper := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatal(errRead)
				}
				if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "ping" {
					t.Errorf("probe input = %q, want ping", got)
				}
				metadata := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
				if got := req.Header.Get("X-Codex-Turn-Metadata"); got != metadata || got == "" {
					t.Errorf("turn metadata header = %q, want body metadata %q", got, metadata)
				}
				outerID := gjson.GetBytes(body, "client_metadata.x-codex-installation-id")
				nestedID := gjson.Get(metadata, "installation_id")
				if tc.wantFixed {
					if outerID.String() != identity.InstallationID || nestedID.String() != identity.InstallationID {
						t.Errorf("probe installation IDs = %q/%q, normal conversation = %q", outerID.String(), nestedID.String(), identity.InstallationID)
					}
				} else if outerID.Exists() || nestedID.Exists() {
					t.Errorf("disabled convergence added installation IDs: %s/%s", outerID.Raw, nestedID.Raw)
				}
				sessionID, turnID := gjson.Get(metadata, "session_id").String(), gjson.Get(metadata, "turn_id").String()
				if sessionID == "" || turnID == "" || sessions[sessionID] || turns[turnID] {
					t.Errorf("probe must use fresh session/turn IDs: %q/%q", sessionID, turnID)
				}
				sessions[sessionID], turns[turnID] = true, true
				if sessionID != req.Header.Get("Session-Id") || sessionID != gjson.GetBytes(body, "client_metadata.session_id").String() {
					t.Error("probe session ID differs between body and headers")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {state}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripper))
			for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-astra"} {
				ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, tc.cfg, auth, model, "")
				if errHarvest != nil || status != http.StatusOK || ticket.State != state {
					t.Fatalf("harvest status=%d ticket length=%d error=%v", status, ticket.Length, errHarvest)
				}
			}
			if len(sessions) != 3 || len(turns) != 3 {
				t.Fatalf("probe requests = %d sessions/%d turns, want 3", len(sessions), len(turns))
			}
		})
	}
}

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexTurnStateTicketResponseRetentionAndReplacement(t *testing.T) {
	for _, eventKind := range []string{"http", "response.metadata", "codex.response.metadata"} {
		t.Run(eventKind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test"}}
				cfg := config.CodexTurnStateTicketConfig{Enabled: true, TTLSeconds: 3600}
				capture := func(state string) bool {
					if eventKind == "http" {
						return RecordCodexTurnStateTicketOnResponse(auth, cfg, "gpt-6-astra", &http.Response{Header: http.Header{CodexTurnStateTicketHeader: {state}}})
					}
					payload, _ := json.Marshal(map[string]any{"type": eventKind, "headers": map[string]string{"x-codex-turn-state": state}})
					return RecordCodexTurnStateTicketOnEvent(auth, cfg, "gpt-6-astra", payload)
				}
				var saves int
				SetCodexTurnStateTicketRecorder(func(_ *cliproxyauth.Auth, _ CodexTurnStateTicket) { saves++ })
				defer SetCodexTurnStateTicketRecorder(nil)
				for _, length := range []int{0, 292, 312, 332} {
					if capture(strings.Repeat("x", length)) || codexTurnStateTicketForAuth(auth, "gpt-6-astra") != nil {
						t.Fatalf("length %d created a retained ticket", length)
					}
				}
				first := strings.Repeat("a", 780)
				if !capture(first) {
					t.Fatal("initial 780 ticket was not saved")
				}
				initial := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
				time.Sleep(20 * time.Minute) // synctest virtual time.
				for _, length := range []int{0, 292, 312, 332} {
					if capture(strings.Repeat("x", length)) {
						t.Fatalf("length %d replaced a retained ticket", length)
					}
					current := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
					if *current != *initial {
						t.Fatalf("length %d changed the ticket or expiry", length)
					}
				}
				headers := make(http.Header)
				if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != first {
					t.Fatal("ticket was not retained across other response lengths")
				}
				second := strings.Repeat("b", 780)
				if !capture(second) {
					t.Fatal("replacement 780 ticket was not saved")
				}
				updated := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
				if updated.State != second || !updated.CapturedAt.Equal(time.Now()) || !updated.ExpiresAt.Equal(initial.ExpiresAt.Add(20*time.Minute)) || saves != 2 {
					t.Fatal("replacement did not restart the retention period exactly once")
				}
				if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != second {
					t.Fatal("header did not switch to the replacement")
				}
				body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{"client_metadata":{"turn_id":"new-turn"}}`))
				if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != second {
					t.Fatal("new turn did not use replacement")
				}
				if !codexTurnStateTicketForAuth(auth, "gpt-6-astra").ExpiresAt.Equal(updated.ExpiresAt) {
					t.Fatal("injection renewed expiry")
				}
				time.Sleep(time.Hour) // synctest virtual time, exactly at expiry.
				headers = make(http.Header)
				if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != "" {
					t.Fatal("expired ticket was injected")
				}
			})
		})
	}
}

func TestCodexTurnStateTicketLegacyValuesAreNotInjected(t *testing.T) {
	for _, length := range []int{292, 312, 332} {
		auth := &cliproxyauth.Auth{ID: "legacy", Provider: "codex", Metadata: map[string]any{"access_token": "test"}}
		StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: testTicketState(length), ExpiresAt: time.Now().Add(time.Hour)})
		cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
		headers := make(http.Header)
		if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil || headers.Get(CodexTurnStateTicketHeader) != "" {
			t.Fatalf("legacy length %d injected", length)
		}
		body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{}`))
		if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
			t.Fatal("legacy ticket injected in body")
		}
		statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
		if len(statuses) != 1 || statuses[0].Ready || statuses[0].TargetLength != 780 || statuses[0].Length != length {
			t.Fatalf("legacy status = %+v", statuses)
		}
	}
}

func TestCodexTurnStateTicketStatusRedactsState(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{"ordinary": "keep"}}
	state := testTicketState(config.DefaultCodexTurnStateTicketTargetLength)
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
	if len(statuses) != 1 || !statuses[0].Ready || statuses[0].TargetLength != 780 || statuses[0].Length != 780 {
		t.Fatalf("statuses = %#v", statuses)
	}
	redacted := RedactCodexTurnStateTicketMetadata(auth.Metadata)
	if _, ok := redacted[CodexTurnStateTicketMetadataKey("gpt-6-astra")]; ok {
		t.Fatal("redacted metadata retained ticket")
	}
	if redacted["ordinary"] != "keep" {
		t.Fatal("redaction removed unrelated metadata")
	}
}

func TestCodexTurnStateTicketUsesFixedLengthForAllPlans(t *testing.T) {
	for _, plan := range []string{"", "free", "plus", "pro", "team", "business"} {
		auth := &cliproxyauth.Auth{ID: "auth-" + plan, Provider: "codex", Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
			"plan_type":                    plan,
		}, Metadata: map[string]any{}}
		state := testTicketState(config.DefaultCodexTurnStateTicketTargetLength)
		StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
		cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, FailClosed: true}
		headers := make(http.Header)
		if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil {
			t.Fatalf("plan=%s apply error = %v", plan, errApply)
		}
		if got := headers.Get(CodexTurnStateTicketHeader); got != state {
			t.Fatalf("plan=%s ticket length=%d, want=%d", plan, len(got), len(state))
		}
		statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
		if len(statuses) != 1 || statuses[0].TargetLength != 780 || statuses[0].Length != 780 {
			t.Fatalf("plan=%s statuses = %#v", plan, statuses)
		}
	}
}

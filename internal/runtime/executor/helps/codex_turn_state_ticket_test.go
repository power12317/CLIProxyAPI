package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func testTicketState(length int) string {
	return "gAAAAA" + strings.Repeat("t", length-len("gAAAAA"))
}

func TestCodexTurnStateTicketAppliesOnlyToConfiguredOAuthModel(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: map[string]any{},
	}
	state := testTicketState(config.DefaultCodexTurnStateTicketPersonalTargetLength)
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
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: testTicketState(292), ExpiresAt: time.Now().Add(-time.Second)})
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
	state := testTicketState(config.DefaultCodexTurnStateTicketPersonalTargetLength)
	var recorded string
	SetCodexTurnStateTicketRecorder(func(gotAuth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
		recorded = gotAuth.ID + "/" + ticket.Model
	})
	defer SetCodexTurnStateTicketRecorder(nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {state}}}
	if invalidated := InvalidateCodexTurnStateTicketOnResponse(auth, cfg, "gpt-6-astra", resp); invalidated {
		t.Fatal("292 response was treated as invalidation")
	}
	ticket := codexTurnStateTicketForAuth(auth, "gpt-6-astra")
	if ticket == nil || !ticket.valid(time.Now(), config.DefaultCodexTurnStateTicketPersonalTargetLength) {
		t.Fatalf("stored response ticket = %#v", ticket)
	}
	if recorded != "auth-response/gpt-6-astra" {
		t.Fatalf("response ticket recorder = %q", recorded)
	}
}

func TestCodexTurnStateTicket312ResponseTriggersProbeWithoutExistingTicket(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-312", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	var invalidated string
	SetCodexTurnStateTicketInvalidator(func(authID, model string) { invalidated = authID + "/" + model })
	defer SetCodexTurnStateTicketInvalidator(nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {strings.Repeat("x", 312)}}}
	if !InvalidateCodexTurnStateTicketOnResponse(auth, cfg, "gpt-6-astra", resp) {
		t.Fatal("312 response did not trigger invalidation/probe")
	}
	if invalidated != "auth-312/gpt-6-astra" {
		t.Fatalf("312 invalidator callback = %q", invalidated)
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
	state := testTicketState(config.DefaultCodexTurnStateTicketPersonalTargetLength)
	cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, AttemptTimeoutSeconds: 2}}}
	roundTripper := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{CodexTurnStateTicketHeader: {state}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripper))
	ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, cfg, auth, "gpt-6-astra", "")
	if errHarvest != nil {
		t.Fatalf("direct harvest error = %v", errHarvest)
	}
	if status != http.StatusOK || ticket.Length != config.DefaultCodexTurnStateTicketPersonalTargetLength {
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
			state := testTicketState(292)
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

func TestCodexTurnStateTicketInvalidatesOnDegraded312Response(t *testing.T) {
	state := testTicketState(config.DefaultCodexTurnStateTicketPersonalTargetLength)
	auth := &cliproxyauth.Auth{ID: "auth-312", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{}}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	var invalidated string
	SetCodexTurnStateTicketInvalidator(func(authID, model string) { invalidated = authID + "/" + model })
	defer SetCodexTurnStateTicketInvalidator(nil)
	resp := &http.Response{Header: http.Header{CodexTurnStateTicketHeader: {strings.Repeat("x", 312)}}}
	if !InvalidateCodexTurnStateTicketOnResponse(auth, cfg, "gpt-6-astra", resp) {
		t.Fatal("312 response did not invalidate the ticket")
	}
	if invalidated != "auth-312/gpt-6-astra" {
		t.Fatalf("invalidator callback = %q", invalidated)
	}
}

func TestCodexTurnStateTicketStatusRedactsState(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{"ordinary": "keep"}}
	state := testTicketState(config.DefaultCodexTurnStateTicketPersonalTargetLength)
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
	if len(statuses) != 1 || !statuses[0].Ready || statuses[0].TargetLength != 292 || statuses[0].Length != 292 {
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

func TestCodexTurnStateTicketUsesTeamBusinessTargetLength(t *testing.T) {
	for _, plan := range []string{"team", "business"} {
		auth := &cliproxyauth.Auth{ID: "auth-" + plan, Provider: "codex", Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
			"plan_type":                    plan,
		}, Metadata: map[string]any{}}
		state := testTicketState(config.DefaultCodexTurnStateTicketTeamTargetLength)
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
		if len(statuses) != 1 || statuses[0].TargetLength != 332 || statuses[0].Length != 332 {
			t.Fatalf("plan=%s statuses = %#v", plan, statuses)
		}
	}
}

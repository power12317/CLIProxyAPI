package helps

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateTicketAppliesOnlyToConfiguredOAuthModel(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: map[string]any{},
	}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: "gAAAAA-ticket", ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, TargetLength: len("gAAAAA-ticket"), Models: []string{"gpt-6-astra"}, FailClosed: true}
	headers := make(http.Header)
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil {
		t.Fatalf("ApplyCodexTurnStateTicket() error = %v", errApply)
	}
	if got := headers.Get(CodexTurnStateTicketHeader); got != "gAAAAA-ticket" {
		t.Fatalf("ticket header = %q", got)
	}
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-5.5", make(http.Header)); errApply != nil {
		t.Fatalf("ungated model was blocked: %v", errApply)
	}
	body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{"client_metadata":{}}`))
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != "gAAAAA-ticket" {
		t.Fatalf("websocket ticket = %q", got)
	}
}

func TestCodexTurnStateTicketFailClosedAndExpiry(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, TargetLength: 12, Models: []string{"gpt-6-astra"}, FailClosed: true}
	errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", make(http.Header))
	if !errors.Is(errApply, ErrCodexTurnStateTicketUnavailable) {
		t.Fatalf("missing ticket error = %v", errApply)
	}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: "gAAAAA-ticket", ExpiresAt: time.Now().Add(-time.Second)})
	if errApply = ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", make(http.Header)); !errors.Is(errApply, ErrCodexTurnStateTicketUnavailable) {
		t.Fatalf("expired ticket error = %v", errApply)
	}
}

func TestCodexTurnStateTicketFailOpenClearsPassiveStateBeforeSending(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}}
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, TargetLength: 12, Models: []string{"gpt-6-astra"}, FailClosed: false}
	headers := http.Header{CodexTurnStateTicketHeader: {"passive-state"}}
	if errApply := ApplyCodexTurnStateTicket(auth, cfg, "gpt-6-astra", headers); errApply != nil {
		t.Fatalf("fail-open ticket apply error = %v", errApply)
	}
	if got := headers.Get(CodexTurnStateTicketHeader); got != "" {
		t.Fatalf("fail-open request retained passive header %q", got)
	}
	body := ApplyCodexTurnStateTicketBody(auth, cfg, "gpt-6-astra", []byte(`{"client_metadata":{"x-codex-turn-state":"passive-state"}}`))
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state"); got.Exists() {
		t.Fatalf("fail-open websocket body retained passive state %s", got.Raw)
	}
}

func TestCodexTurnStateTicketInvalidatesOnDegraded312Response(t *testing.T) {
	state := "gAAAAA-ticket"
	auth := &cliproxyauth.Auth{ID: "auth-312", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth}, Metadata: map[string]any{}}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: state, ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, TargetLength: len(state), Models: []string{"gpt-6-astra"}}
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
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "gpt-6-astra", State: "gAAAAA-ticket", ExpiresAt: time.Now().Add(time.Hour)})
	cfg := config.CodexTurnStateTicketConfig{Enabled: true, TargetLength: len("gAAAAA-ticket"), Models: []string{"gpt-6-astra"}}
	statuses := CodexTurnStateTicketStatuses(auth, cfg, time.Now())
	if len(statuses) != 1 || !statuses[0].Ready || statuses[0].Length != len("gAAAAA-ticket") {
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

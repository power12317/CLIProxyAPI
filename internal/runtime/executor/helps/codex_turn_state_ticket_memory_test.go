package helps

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexTicketMemoryIgnoresDiskAndCopiesCookiePair(t *testing.T) {
	auth := ticketProbeTestAuth(t.Name(), "user@example.com", "account")
	t.Cleanup(func() { InvalidateCodexTurnStateTickets(auth.ID) })
	now := time.Now().Truncate(time.Second)
	route := borrowTestRoute()
	ticket := CodexTurnStateTicket{Model: "gpt-6-astra", State: borrowTestState(now, 1), Length: 780, CapturedAt: now, ExpiresAt: now.Add(time.Hour), Route: &route}
	auth.Metadata[CodexTurnStateTicketMetadataKey(ticket.Model)] = ticket
	if codexTurnStateTicketForAuth(auth, ticket.Model) != nil {
		t.Fatal("legacy auth metadata restored a ticket into memory")
	}
	delete(auth.Metadata, CodexTurnStateTicketMetadataKey(ticket.Model))
	before, _ := json.Marshal(auth.Metadata)
	StoreCodexTurnStateTicket(auth, ticket)
	after, _ := json.Marshal(auth.Metadata)
	if string(before) != string(after) {
		t.Fatal("memory store mutated auth metadata")
	}
	route.CFLB.Value = "mutated-input"
	first, err := ExportCodexSourceTicket(auth.Clone(), ticket.Model, now)
	if err != nil || first.Route.CFLB.Value != "source-cf" {
		t.Fatalf("input cookie mutation altered snapshot: %v", err)
	}
	first.Route.OaiLB.Value = "mutated-output"
	second, err := ExportCodexSourceTicket(auth, ticket.Model, now)
	if err != nil || second.Route.OaiLB.Value != "source-oai" {
		t.Fatalf("output cookie mutation altered snapshot: %v", err)
	}
	InvalidateCodexTurnStateTickets(auth.ID)
	auth.Metadata[CodexTurnStateTicketMetadataKey(ticket.Model)] = ticket
	if _, err := ExportCodexSourceTicket(auth, ticket.Model, now); err == nil {
		t.Fatal("cleared cache silently fell back to old JSON metadata")
	}
}

func TestCodexTicketMemoryIsolatesCredentialsModelsAndOwners(t *testing.T) {
	a := ticketProbeTestAuth(t.Name()+"-mac", "same@example.com", "same-account")
	b := ticketProbeTestAuth(t.Name()+"-windows", "same@example.com", "same-account")
	b.Metadata["codex_client_system"] = "windows"
	t.Cleanup(func() { InvalidateCodexTurnStateTickets(a.ID); InvalidateCodexTurnStateTickets(b.ID) })
	now := time.Now()
	for i, auth := range []*cliproxyauth.Auth{a, b} {
		for j, model := range []string{"gpt-6-astra", "gpt-5.6-luna"} {
			StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: model, State: borrowTestState(now, byte(1+i*2+j)), CapturedAt: now, ExpiresAt: now.Add(time.Hour)})
		}
	}
	if codexTurnStateTicketForAuth(a, "gpt-6-astra").State == codexTurnStateTicketForAuth(b, "gpt-6-astra").State || codexTurnStateTicketForAuth(a, "gpt-6-astra").State == codexTurnStateTicketForAuth(a, "gpt-5.6-luna").State {
		t.Fatal("credentials or models shared tickets")
	}
	refreshed := a.Clone()
	refreshed.Metadata["access_token"] = "refreshed-token"
	if codexTurnStateTicketForAuth(refreshed, "gpt-6-astra") == nil {
		t.Fatal("ordinary token refresh lost the same owner's ticket")
	}
	refreshed.Metadata["account_id"] = "different-account"
	if codexTurnStateTicketForAuth(refreshed, "gpt-6-astra") != nil {
		t.Fatal("another owner inherited a ticket")
	}
	InvalidateCodexTurnStateTickets(a.ID)
	if codexTurnStateTicketForAuth(a, "gpt-6-astra") != nil || codexTurnStateTicketForAuth(b, "gpt-6-astra") == nil {
		t.Fatal("credential removal invalidated the wrong cache")
	}
}

func TestCodexTicketMemoryConcurrentObservationKeepsNewestAndOriginalTTL(t *testing.T) {
	auth := ticketProbeTestAuth(t.Name(), "user@example.com", "account")
	t.Cleanup(func() { InvalidateCodexTurnStateTickets(auth.ID) })
	now := time.Now().Truncate(time.Second)
	route := borrowTestRoute()
	newest := CodexTurnStateTicket{Model: "model", State: borrowTestState(now, 2), CapturedAt: now, ExpiresAt: now.Add(time.Hour), Route: &route}
	older := newest
	older.State, older.CapturedAt, older.ExpiresAt = borrowTestState(now.Add(-time.Minute), 1), now.Add(-time.Minute), now.Add(59*time.Minute)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			ticket := newest
			if i%2 == 0 {
				ticket = older
			}
			storeCodexTicketMemory(auth.Clone(), ticket, true)
			copy := codexTurnStateTicketForAuth(auth, "model")
			copy.Route.CFLB.Value = "private-copy"
		})
	}
	wg.Wait()
	got := codexTurnStateTicketForAuth(auth, "model")
	if got.State != newest.State || got.Route.CFLB.Value != route.CFLB.Value {
		t.Fatal("concurrent older snapshot or reader overwrote the current pair")
	}
	newest.ExpiresAt = now.Add(2 * time.Hour)
	storeCodexTicketMemory(auth, newest, true)
	if !codexTurnStateTicketForAuth(auth, "model").ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("same ticket extended the original hour")
	}
}

func TestCodexTicketMemoryRejectsLateResponseAfterRemoval(t *testing.T) {
	auth := ticketProbeTestAuth(t.Name(), "user@example.com", "account")
	t.Cleanup(func() { InvalidateCodexTurnStateTickets(auth.ID) })
	manager := cliproxyauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	h := NewCodexTurnStateTicketHarvester(CodexTurnStateTicketHarvesterOptions{List: manager.List})
	SetCodexTurnStateTicketRecorder(h.Record)
	t.Cleanup(func() { SetCodexTurnStateTicketRecorder(nil) })
	manager.Remove(t.Context(), auth.ID)
	resp := &http.Response{Header: http.Header{CodexTurnStateTicketHeader: {strings.Repeat("t", 780)}}}
	RecordCodexTurnStateTicketOnResponse(registered, config.CodexTurnStateTicketConfig{Enabled: true}, "model", resp)
	if codexTurnStateTicketForAuth(registered, "model") != nil {
		t.Fatal("late response bypassed the service's removed-credential check")
	}
	if !reflect.DeepEqual(registered.Metadata, auth.Metadata) {
		t.Fatal("late response modified credential metadata")
	}
	// Even a write already in flight under the old epoch cannot be read by a
	// replacement registration of the same credential ID and owner.
	replacement, err := manager.Register(t.Context(), auth.Clone())
	if err != nil {
		t.Fatal(err)
	}
	StoreCodexTurnStateTicket(registered, CodexTurnStateTicket{Model: "model", State: strings.Repeat("t", 780), ExpiresAt: time.Now().Add(time.Hour)})
	if codexTurnStateTicketForAuth(replacement, "model") != nil {
		t.Fatal("replacement registration inherited a late ticket")
	}
}

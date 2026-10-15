package helps

import (
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codexTicketMemoryKey struct {
	authID string
	owner  string
	epoch  uint64
}

var codexTicketMemory = struct {
	sync.RWMutex
	entries map[codexTicketMemoryKey]map[string]CodexTurnStateTicket
	pruneAt time.Time
}{entries: make(map[codexTicketMemoryKey]map[string]CodexTurnStateTicket)}

func codexTicketKey(auth *cliproxyauth.Auth) codexTicketMemoryKey {
	return codexTicketMemoryKey{authID: auth.ID, owner: CodexOwnerFingerprint(auth), epoch: auth.RegistrationEpoch}
}

func cloneCodexTicket(ticket CodexTurnStateTicket) CodexTurnStateTicket {
	if ticket.Route != nil {
		route := *ticket.Route
		ticket.Route = &route
	}
	return ticket
}

func codexTurnStateTicketForAuth(auth *cliproxyauth.Auth, model string) *CodexTurnStateTicket {
	if auth == nil || auth.ID == "" || strings.TrimSpace(model) == "" {
		return nil
	}
	key := codexTicketKey(auth)
	codexTicketMemory.RLock()
	ticket, ok := codexTicketMemory.entries[key][strings.TrimSpace(model)]
	codexTicketMemory.RUnlock()
	if !ok {
		return nil
	}
	ticket = cloneCodexTicket(ticket)
	return &ticket
}

func codexTurnStateTicketModels(auth *cliproxyauth.Auth) []string {
	key := codexTicketKey(auth)
	codexTicketMemory.RLock()
	defer codexTicketMemory.RUnlock()
	models := make([]string, 0, len(codexTicketMemory.entries[key]))
	for model := range codexTicketMemory.entries[key] {
		models = append(models, model)
	}
	return models
}

// StoreCodexTurnStateTicket replaces a process-local snapshot. It never mutates
// auth metadata or calls the credential store, including for routing cookies.
func StoreCodexTurnStateTicket(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
	storeCodexTicketMemory(auth, ticket, false)
}

func storeCodexTicketMemory(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket, observed bool) {
	if auth == nil || auth.ID == "" || strings.TrimSpace(ticket.Model) == "" || strings.TrimSpace(ticket.State) == "" {
		return
	}
	key := codexTicketKey(auth)
	now := time.Now()
	ticket.AccountID, ticket.Owner = auth.ID, key.owner
	ticket.Model, ticket.State = strings.TrimSpace(ticket.Model), strings.TrimSpace(ticket.State)
	ticket.Length = len(ticket.State)
	issued, err := CodexTicketIssuedAt(ticket.State, now)
	if err != nil {
		issued = ticket.CapturedAt
	}
	if !issued.IsZero() && ticket.ExpiresAt.After(issued.Add(time.Hour)) {
		ticket.ExpiresAt = issued.Add(time.Hour)
	}
	codexTicketMemory.Lock()
	defer codexTicketMemory.Unlock()
	if !now.Before(codexTicketMemory.pruneAt) {
		for k, models := range codexTicketMemory.entries {
			for model, previous := range models {
				// Keep a bounded expired-state window for the credential status page.
				if previous.ExpiresAt.Before(now.Add(-time.Hour)) {
					delete(models, model)
				}
			}
			if len(models) == 0 {
				delete(codexTicketMemory.entries, k)
			}
		}
		codexTicketMemory.pruneAt = now.Add(time.Minute)
	}
	models := codexTicketMemory.entries[key]
	if models == nil {
		models = make(map[string]CodexTurnStateTicket)
		codexTicketMemory.entries[key] = models
	}
	if previous, ok := models[ticket.Model]; observed && ok {
		if previous.CapturedAt.After(ticket.CapturedAt) {
			return
		}
		if previous.State == ticket.State {
			ticket.CapturedAt, ticket.ExpiresAt = previous.CapturedAt, previous.ExpiresAt
			if previous.Route != nil && (previous.Route.valid(now) || ticket.Route == nil || previous.Route.sameValues(*ticket.Route)) {
				ticket.Route = previous.Route
			}
		} else if ticket.Route == nil && previous.valid(now) && previous.Route != nil && previous.Route.valid(now) {
			return
		}
	}
	models[ticket.Model] = cloneCodexTicket(ticket)
}

// InvalidateCodexTurnStateTickets releases snapshots when a credential is removed
// or replaced. Ordinary reloads and token refreshes retain the same owner's cache.
func InvalidateCodexTurnStateTickets(authID string) {
	codexTicketMemory.Lock()
	defer codexTicketMemory.Unlock()
	for key := range codexTicketMemory.entries {
		if key.authID == authID {
			delete(codexTicketMemory.entries, key)
		}
	}
}

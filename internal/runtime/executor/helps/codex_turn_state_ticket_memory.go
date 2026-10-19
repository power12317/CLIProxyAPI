package helps

import (
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codexTicketMemoryKey struct {
	id, owner string
	epoch     uint64
}

var codexTicketMemory = struct {
	sync.RWMutex
	entries map[codexTicketMemoryKey]map[string]CodexTurnStateTicket
}{entries: make(map[codexTicketMemoryKey]map[string]CodexTurnStateTicket)}

func codexTicketKey(auth *cliproxyauth.Auth) codexTicketMemoryKey {
	return codexTicketMemoryKey{id: auth.ID, owner: CodexOwnerFingerprint(auth), epoch: auth.RegistrationEpoch}
}

func codexTurnStateTicketForAuth(auth *cliproxyauth.Auth, model string) *CodexTurnStateTicket {
	if auth == nil {
		return nil
	}
	codexTicketMemory.RLock()
	ticket, ok := codexTicketMemory.entries[codexTicketKey(auth)][strings.TrimSpace(model)]
	codexTicketMemory.RUnlock()
	if !ok {
		return nil
	}
	return &ticket
}

func codexTurnStateTicketModels(auth *cliproxyauth.Auth) []string {
	if auth == nil {
		return nil
	}
	codexTicketMemory.RLock()
	defer codexTicketMemory.RUnlock()
	var models []string
	for model := range codexTicketMemory.entries[codexTicketKey(auth)] {
		models = append(models, model)
	}
	return models
}

// StoreCodexTurnStateTicket updates only process memory, never Auth.Metadata or a store.
func StoreCodexTurnStateTicket(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
	if auth == nil || auth.ID == "" || strings.TrimSpace(ticket.Model) == "" || strings.TrimSpace(ticket.State) == "" {
		return
	}
	ticket.AccountID = auth.ID
	ticket.Model = strings.TrimSpace(ticket.Model)
	ticket.State = strings.TrimSpace(ticket.State)
	ticket.Length = len(ticket.State)
	key := codexTicketKey(auth)
	codexTicketMemory.Lock()
	defer codexTicketMemory.Unlock()
	if codexTicketMemory.entries[key] == nil {
		codexTicketMemory.entries[key] = make(map[string]CodexTurnStateTicket)
	}
	previous, exists := codexTicketMemory.entries[key][ticket.Model]
	if exists && previous.CapturedAt.After(ticket.CapturedAt) {
		return
	}
	codexTicketMemory.entries[key][ticket.Model] = ticket
}

func InvalidateCodexTurnStateTickets(authID string) {
	codexTicketMemory.Lock()
	defer codexTicketMemory.Unlock()
	for key := range codexTicketMemory.entries {
		if key.id == authID {
			delete(codexTicketMemory.entries, key)
		}
	}
}

func pruneCodexTurnStateTickets(now time.Time) {
	codexTicketMemory.Lock()
	defer codexTicketMemory.Unlock()
	for key, models := range codexTicketMemory.entries {
		for model, ticket := range models {
			if !ticket.ExpiresAt.IsZero() && ticket.ExpiresAt.Before(now.Add(-time.Hour)) {
				delete(models, model)
			}
		}
		if len(models) == 0 {
			delete(codexTicketMemory.entries, key)
		}
	}
}

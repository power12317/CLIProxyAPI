// Package credentialmeta defines credential fields that must never be persisted.
package credentialmeta

import "strings"

const TurnTicketPrefix = "codex_turn_ticket:"

// StripTurnTickets removes legacy tickets, including any nested routing snapshot.
func StripTurnTickets(metadata map[string]any) {
	for key := range metadata {
		if strings.HasPrefix(strings.TrimSpace(key), TurnTicketPrefix) {
			delete(metadata, key)
		}
	}
}

// WithoutTurnTickets copies persistent metadata without mutating its caller.
func WithoutTurnTickets(metadata map[string]any) map[string]any {
	copy := make(map[string]any, len(metadata))
	for key, value := range metadata {
		copy[key] = value
	}
	StripTurnTickets(copy)
	return copy
}

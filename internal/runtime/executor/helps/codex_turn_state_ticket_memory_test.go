package helps

import (
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestTicketMemoryNeverUsesAuthMetadata(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", RegistrationEpoch: 1, Metadata: map[string]any{"email": "synthetic@example.invalid", CodexTurnStateTicketMetadataKey("legacy"): map[string]any{"state": strings.Repeat("a", 780)}}}
	t.Cleanup(func() { InvalidateCodexTurnStateTickets(auth.ID) })
	before := auth.Clone()
	if codexTurnStateTicketForAuth(auth, "legacy") != nil {
		t.Fatal("loaded disk ticket")
	}
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "model-a", State: strings.Repeat("a", 780), CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	StoreCodexTurnStateTicket(auth, CodexTurnStateTicket{Model: "model-b", State: strings.Repeat("b", 780), CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if !reflect.DeepEqual(auth.Metadata, before.Metadata) {
		t.Fatal("memory caching mutated credential")
	}
	if codexTurnStateTicketForAuth(auth.Clone(), "model-a") == nil || len(codexTurnStateTicketModels(auth)) != 2 {
		t.Fatal("memory ticket not shared with snapshots")
	}
	for _, changed := range []string{"id", "owner", "epoch"} {
		other := auth.Clone()
		switch changed {
		case "id":
			other.ID += "-windows"
		case "owner":
			other.Metadata["email"] = "other@example.invalid"
		case "epoch":
			other.RegistrationEpoch++
		}
		if codexTurnStateTicketForAuth(other, "model-a") != nil {
			t.Fatalf("ticket leaked across %s", changed)
		}
	}
	InvalidateCodexTurnStateTickets(auth.ID)
	if codexTurnStateTicketForAuth(auth, "model-a") != nil {
		t.Fatal("removed credential retained tickets")
	}
}

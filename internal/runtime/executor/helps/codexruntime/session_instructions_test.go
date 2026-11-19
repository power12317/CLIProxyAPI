package codexruntime

import (
	"context"
	"errors"
	"testing"

	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagedParentStaysWithItsIssuingWorker(t *testing.T) {
	var route managedRoute
	a := managedOwner{"ws://one", "credential-A", "execution-A"}
	b := managedOwner{"ws://two", "credential-B", "execution-B"}
	full := &SessionInstructions{Mode: "start", Scope: t.Name(), BaselineDigest: "digest"}
	parent := &SessionInstructions{Mode: "reuse", Scope: t.Name(), BaselineDigest: "digest", PreviousResponseID: "R1"}
	begin := func(owner managedOwner, control *SessionInstructions) *Client {
		t.Helper()
		p, err := route.begin(owner, control)
		if err != nil {
			t.Fatal(err)
		}
		return &Client{ctx: t.Context(), receipt: p}
	}
	reject := func(owner managedOwner, control *SessionInstructions) {
		t.Helper()
		_, err := route.begin(owner, control)
		var loss *core.UpstreamWebsocketTurnError
		if !errors.As(err, &loss) || !loss.FullContextRequired || loss.StatusCode() != 409 {
			t.Fatalf("expected 409 for %v, got %v", owner, err)
		}
	}
	deliver := func(client *Client, id string) {
		client.ObserveManagedEvent([]byte(`{"type":"response.created","response":{"id":"` + id + `"}}`))
		client.ObserveManagedEvent([]byte(`{"type":"response.completed","response":{"status":"completed","id":"` + id + `"}}`))
		client.CompleteManagedDelivery()
	}
	first := begin(a, full)
	reject(a, parent)
	deliver(first, "R1")
	reject(b, parent)
	reject(managedOwner{a.endpoint, a.credential, "other session"}, parent)
	next := begin(a, parent)
	recovery := begin(b, full)
	deliver(next, "late")
	deliver(recovery, "R2")
	reject(a, parent)
	parent.PreviousResponseID = "R2"
	reject(a, parent)
	duplicate := begin(b, parent)
	deliver(duplicate, "R2")
	reject(b, parent)
	pending := begin(a, full)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	pending.ctx = cancelled
	deliver(pending, "R3")
	parent.PreviousResponseID = "R3"
	reject(a, parent)
}

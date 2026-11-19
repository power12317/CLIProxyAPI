package codexruntime

import (
	"strings"
	"sync"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Logical baselines may be shared through KV. Delivery receipts stay in this
// CPA process: another instance cannot attest to a response it never saw.
var managedRoutes = cache.NewBoundedLRU[string, *managedRoute](512, nil)

func HasManagedRoute(scope string) bool {
	_, ok := managedRoutes.Get(scope)
	return ok
}

func ForgetManagedRoute(scope string) { managedRoutes.Delete(scope) }

type managedOwner struct {
	endpoint, credential, session string
}

type managedRoute struct {
	mu         sync.Mutex
	current    *managedReceipt
	previousID string
}

type managedReceipt struct {
	route       *managedRoute
	owner       managedOwner
	digest      string
	responseID  string
	completedID string
	createdID   string
	invalid     bool
}

func missingManagedParent() error {
	return managedTurnError(&core.UpstreamWebsocketTurnError{FullContextRequired: true})
}

func managedTurnError(turn *core.UpstreamWebsocketTurnError) error {
	body := turn.Error()
	return &Error{Status: turn.StatusCode(), Message: body, Body: []byte(body), cause: turn}
}

func (r *managedRoute) matches(owner managedOwner, control *SessionInstructions) bool {
	last := r.current
	return last != nil && last.responseID != "" && last.owner == owner && last.digest != "" &&
		last.digest == control.BaselineDigest && (control.PreviousResponseID == "" || control.PreviousResponseID == last.responseID)
}

// CheckManagedParent runs before dialing a newly selected worker. Start checks
// again under the same route lock in case another turn intervened during dial.
func CheckManagedParent(control *SessionInstructions, endpoint, credential, session string) error {
	if !control.Incremental() {
		return nil
	}
	if control.HTTPOnly {
		return missingManagedParent()
	}
	route, ok := managedRoutes.Get(control.Scope)
	if ok {
		route.mu.Lock()
		ok = route.matches(managedOwner{endpoint, credential, session}, control)
		route.mu.Unlock()
	}
	if !ok {
		return missingManagedParent()
	}
	return nil
}

// begin invalidates the previous proof only when starting an accepted turn. A
// misrouted delta cannot consume the original worker's valid receipt.
func (r *managedRoute) begin(owner managedOwner, control *SessionInstructions) (*managedReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if control.Incremental() && !r.matches(owner, control) {
		return nil, missingManagedParent()
	}
	receipt := &managedReceipt{route: r, owner: owner, digest: control.BaselineDigest}
	r.current = receipt
	return receipt, nil
}

// The worker independently verifies created/completed and the physical socket.
// CPA records only the unmodified response ID it actually received for delivery.
func (c *Client) ObserveManagedEvent(event []byte) {
	p := c.receipt
	if p == nil || p.invalid {
		return
	}
	root := ojson.GetBytesNoCopy(event, "@this")
	switch root.Get("type").String() {
	case "response.created":
		id := root.Get("response.id")
		if p.createdID != "" || id.Type != ojson.String || id.String() == "" || len(id.String()) > 256 {
			p.invalid = true
			return
		}
		p.createdID = strings.Clone(id.String())
	case "response.completed", "response.done":
		id := root.Get("response.id")
		status := root.Get("response.status").String()
		if id.Type != ojson.String || p.createdID == "" || id.String() != p.createdID || (status != "" && status != "completed") {
			p.invalid = true
			return
		}
		p.completedID = p.createdID
	case "response.incomplete", "response.failed", "error":
		p.invalid = true
	}
}

func (c *Client) CompleteManagedDelivery() {
	p := c.receipt
	if p == nil || p.invalid || p.completedID == "" || c.ctx.Err() != nil {
		return
	}
	p.route.mu.Lock()
	defer p.route.mu.Unlock()
	// A late completion or a second response with the same literal ID cannot
	// establish a new unambiguous parent.
	if p.route.current == p {
		if p.completedID != p.route.previousID {
			p.responseID = p.completedID
		}
		p.route.previousID = p.completedID
	}
}

func (c *Client) AbandonManagedDelivery() {
	p := c.receipt
	if p == nil {
		return
	}
	p.route.mu.Lock()
	defer p.route.mu.Unlock()
	if p.route.current == p {
		p.responseID = ""
	}
}

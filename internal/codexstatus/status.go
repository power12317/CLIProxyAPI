// Package codexstatus detects Codex degradation and keeps process-local state.
package codexstatus

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const Header = "X-CPA-Status"
const StreamPrefix = ": cpa-status "

type Credential struct{ ID, Owner, File, Email string }
type credentialKey struct{ id, owner string }

type Monitor struct {
	mu       sync.Mutex
	cfg      config.EventNotifications
	turns    map[string]time.Time
	notified map[credentialKey]time.Time
	now      func() time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	send     func(context.Context, config.EventNotifications, string) error
}

func New(cfg config.EventNotifications) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{cfg: cfg, turns: make(map[string]time.Time), notified: make(map[credentialKey]time.Time), now: time.Now, ctx: ctx, cancel: cancel, send: sendNotification}
}

func (m *Monitor) Update(cfg config.EventNotifications) { m.mu.Lock(); m.cfg = cfg; m.mu.Unlock() }
func (m *Monitor) Close()                               { m.cancel() }
func (m *Monitor) Request() *Request                    { return &Request{monitor: m} }

type Request struct {
	monitor        *Monitor
	negotiated     bool
	seen, degraded bool
	version, round uint64
}
type Attempt struct {
	request           *Request
	credential        Credential
	eligible, matched bool
	round             uint64
}
type contextKey struct{}

func WithRequest(ctx context.Context, request *Request) context.Context {
	return context.WithValue(ctx, contextKey{}, request)
}
func FromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Request)
	return r
}

// Reset gives each websocket request an independent result.
func (r *Request) Reset() {
	if r == nil {
		return
	}
	r.monitor.mu.Lock()
	defer r.monitor.mu.Unlock()
	r.seen, r.degraded = false, false
	r.round++
}

func (r *Request) statusLocked() string {
	if !r.seen {
		return ""
	}
	if r.degraded {
		return "degraded"
	}
	return "normal"
}
func (r *Request) Status() string {
	r.monitor.mu.Lock()
	defer r.monitor.mu.Unlock()
	return r.statusLocked()
}
func (r *Request) WebsocketStatus(lastVersion uint64) (uint64, []byte) {
	if r == nil {
		return lastVersion, nil
	}
	r.monitor.mu.Lock()
	defer r.monitor.mu.Unlock()
	if !r.negotiated || !r.seen || r.version == lastVersion {
		return lastVersion, nil
	}
	return r.version, []byte(`{"type":"cpa.status","status":"` + r.statusLocked() + `"}`)
}

func (r *Request) Begin(c Credential, turn string, length int) *Attempt {
	m := r.monitor
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	eligible := false
	if turn != "" {
		first, exists := m.turns[turn]
		if !exists {
			first = now
			m.turns[turn] = first
		}
		age := now.Sub(first)
		eligible = length == 780 && age >= 0 && age <= time.Hour
	}
	r.seen = true
	r.version++
	return &Attempt{request: r, credential: c, eligible: eligible, round: r.round}
}

func (a *Attempt) Observe(length int) {
	if a == nil || !a.eligible || length != 780 {
		return
	}
	r := a.request
	m := r.monitor
	m.mu.Lock()
	if a.matched {
		m.mu.Unlock()
		return
	}
	a.matched = true
	if r.round == a.round {
		r.degraded = true
		r.version++
	}
	m.mu.Unlock()
	m.notify(a.credential)
}

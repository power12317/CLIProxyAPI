package helps

import (
	"context"
	"sync"

	"github.com/tidwall/gjson"
)

// CodexWebsocketActivity tracks only the native response boundary of one topic.
// It never routes events between conversations. Cancellation leaves the reader
// draining a response before another consumer can acquire the topic's socket.
type CodexWebsocketActivity struct {
	once         sync.Once
	gate         chan struct{}
	mu           sync.Mutex
	changed      chan struct{}
	pending      []string
	active       map[string]struct{}
	steers       map[string]int
	accepted     map[string]string
	waiting      map[string]bool
	lastResponse string
}

func (a *CodexWebsocketActivity) Acquire(ctx context.Context) error {
	a.once.Do(func() { a.gate = make(chan struct{}, 1) })
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	for {
		a.mu.Lock()
		busy := len(a.pending) > 0 || len(a.active) > 0 || len(a.steers) > 0
		for _, parent := range a.accepted {
			busy = busy || !a.waiting[parent]
		}
		if a.changed == nil {
			a.changed = make(chan struct{})
		}
		changed := a.changed
		a.mu.Unlock()
		if err := ctx.Err(); err != nil {
			a.Release()
			return err
		}
		if !busy {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			a.Release()
			return ctx.Err()
		}
	}
}

func (a *CodexWebsocketActivity) Release() { <-a.gate }

func (a *CodexWebsocketActivity) Sent(payload []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	parent := gjson.GetBytes(payload, "previous_response_id").String()
	switch gjson.GetBytes(payload, "type").String() {
	case "response.create":
		a.consumeSuccessor(parent)
		a.pending = append(a.pending, parent)
	case "response.steer":
		if a.steers == nil {
			a.steers = make(map[string]int)
		}
		a.steers[parent]++
	}
}

func (a *CodexWebsocketActivity) consumeSuccessor(parent string) {
	if parent == "" {
		return
	}
	for id, target := range a.accepted {
		if target == parent {
			delete(a.accepted, id)
		}
	}
	delete(a.waiting, parent)
}

func (a *CodexWebsocketActivity) Received(payload []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := gjson.GetBytes(payload, "response.id").String()
	switch gjson.GetBytes(payload, "type").String() {
	case "response.created":
		parent := gjson.GetBytes(payload, "response.previous_response_id").String()
		if len(a.pending) > 0 {
			if parent == "" {
				parent = a.pending[0]
			}
			a.pending = a.pending[1:]
		} else if parent == "" {
			parent = a.lastResponse
		}
		a.consumeSuccessor(parent)
		if a.active == nil {
			a.active = make(map[string]struct{})
		}
		a.active[id] = struct{}{}
		a.lastResponse = id
	case "response.steer.accepted", "response.steer.failed":
		parent := gjson.GetBytes(payload, "steer.previous_response_id").String()
		steer := gjson.GetBytes(payload, "steer.id").String()
		if a.steers[parent] > 1 {
			a.steers[parent]--
		} else {
			delete(a.steers, parent)
		}
		if gjson.GetBytes(payload, "type").String() == "response.steer.accepted" {
			if a.accepted == nil {
				a.accepted = make(map[string]string)
			}
			a.accepted[steer] = parent
		} else {
			delete(a.accepted, steer)
		}
	case "response.steer.pending":
		if gjson.GetBytes(payload, "reason").String() == "waiting_for_required_input" {
			if a.waiting == nil {
				a.waiting = make(map[string]bool)
			}
			a.waiting[gjson.GetBytes(payload, "steer.previous_response_id").String()] = true
		}
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
		if id == "" {
			id = gjson.GetBytes(payload, "response_id").String()
		}
		if _, ok := a.active[id]; ok {
			delete(a.active, id)
		} else if len(a.pending) > 0 {
			a.pending = a.pending[1:]
		} else if id == "" && len(a.active) == 1 {
			clear(a.active)
		}
	default:
		return
	}
	a.notify()
}

func (a *CodexWebsocketActivity) notify() {
	if a.changed != nil {
		close(a.changed)
	}
	a.changed = make(chan struct{})
}

func (a *CodexWebsocketActivity) Disconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = nil
	clear(a.active)
	clear(a.steers)
	clear(a.accepted)
	clear(a.waiting)
	a.lastResponse = ""
	a.notify()
}

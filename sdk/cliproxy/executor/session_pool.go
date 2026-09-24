package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/google/uuid"
)

// ExecutionSessionPool keeps a bounded number of idle sessions. Active requests
// never share a lease. It performs no network I/O and has no reconnect timer.
type ExecutionSessionPool struct {
	mu           sync.Mutex
	idle         []executionSessionSlot
	closeSession func(string)
	generation   uint64
}

type executionSessionSlot struct{ key, id string }

const maxIdleExecutionSessions = 128

// A missing authenticated caller scope disables reuse across HTTP requests.
func CodexSessionPoolKey(req Request, opts Options) string {
	caller, _ := opts.Metadata[CallerScopeMetadataKey].(string)
	if caller == "" {
		return ""
	}
	encoded, _ := json.Marshal([]any{caller, opts.Metadata[CanonicalSessionIDMetadataKey], req.Model, opts.ProxyURL})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func NewExecutionSessionPool(closeSession func(string)) *ExecutionSessionPool {
	return &ExecutionSessionPool{closeSession: closeSession}
}

func (p *ExecutionSessionPool) Acquire(key string) (string, func()) {
	p.mu.Lock()
	generation := p.generation
	slot := executionSessionSlot{key: key}
	if key != "" {
		for i := len(p.idle) - 1; i >= 0; i-- {
			if p.idle[i].key == key {
				slot = p.idle[i]
				p.idle = append(p.idle[:i], p.idle[i+1:]...)
				break
			}
		}
	}
	p.mu.Unlock()
	if slot.id == "" {
		slot.id = "codex-forced/" + uuid.NewString()
	}
	var once sync.Once
	return slot.id, func() {
		once.Do(func() {
			if key == "" {
				p.closeSession(slot.id)
				return
			}
			var evicted string
			p.mu.Lock()
			if p.generation != generation {
				p.mu.Unlock()
				p.closeSession(slot.id)
				return
			}
			p.idle = append(p.idle, slot)
			if len(p.idle) > maxIdleExecutionSessions {
				evicted = p.idle[0].id
				p.idle = p.idle[1:]
			}
			p.mu.Unlock()
			if evicted != "" {
				p.closeSession(evicted)
			}
		})
	}
}

// Drain retires idle sessions immediately and active leases when they return.
func (p *ExecutionSessionPool) Drain() {
	p.mu.Lock()
	p.generation++
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, slot := range idle {
		p.closeSession(slot.id)
	}
}

// ReleaseSessionAfterStream transfers a lease to the stream consumer. Cancellation
// releases it as well; executors must invalidate a cancelled in-flight connection.
func ReleaseSessionAfterStream(ctx context.Context, result *StreamResult, release func()) *StreamResult {
	if release == nil {
		return result
	}
	if result == nil || result.Chunks == nil {
		release()
		return result
	}
	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		defer release()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return &StreamResult{Headers: result.Headers, Chunks: out}
}

package logging

import (
	"context"
	"sync"
)

type diagnosticFinalizersKey struct{}

type diagnosticFinalizers struct {
	mu     sync.Mutex
	finish []func()
	closed bool
}

// WithDiagnosticFinalizers keeps asynchronous runtime logs inside the request
// lifetime. The consumer flushes business output before joining these tasks.
func WithDiagnosticFinalizers(ctx context.Context) (context.Context, func(func())) {
	state := &diagnosticFinalizers{}
	return context.WithValue(ctx, diagnosticFinalizersKey{}, state), func(flush func()) {
		state.mu.Lock()
		finish := state.finish
		state.finish = nil
		state.closed = true
		state.mu.Unlock()
		if len(finish) == 0 {
			return
		}
		flush()
		for _, fn := range finish {
			fn()
		}
	}
}

// DeferDiagnostics returns the previous attempt's finalizer so log consumers
// can preserve request/response ordering without delaying business delivery.
func DeferDiagnostics(ctx context.Context, finish func()) (func(), bool) {
	state, _ := ctx.Value(diagnosticFinalizersKey{}).(*diagnosticFinalizers)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, false
	}
	var previous func()
	if len(state.finish) > 0 {
		previous = state.finish[len(state.finish)-1]
	}
	state.finish = append(state.finish, finish)
	return previous, true
}

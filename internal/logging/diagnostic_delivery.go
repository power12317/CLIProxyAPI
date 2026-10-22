package logging

import (
	"context"
	"sync"
)

type diagnosticFinalizersKey struct{}

type diagnosticFinalizers struct {
	mu         sync.Mutex
	finish     []func()
	closed     bool
	background bool
	previous   func()
	pending    sync.WaitGroup
}

// WithDiagnosticFinalizers keeps asynchronous runtime logs inside the request
// lifetime. The consumer flushes business output before joining these tasks.
func WithDiagnosticFinalizers(ctx context.Context) (context.Context, func(func())) {
	return withDiagnosticFinalizers(ctx, false)
}

// WithBackgroundDiagnosticFinalizers lets successive WebSocket requests proceed
// while prior diagnostics finish, and joins log tasks before the socket handler exits.
func WithBackgroundDiagnosticFinalizers(ctx context.Context) (context.Context, func(func())) {
	return withDiagnosticFinalizers(ctx, true)
}

func withDiagnosticFinalizers(ctx context.Context, background bool) (context.Context, func(func())) {
	if _, ok := ctx.Value(diagnosticFinalizersKey{}).(*diagnosticFinalizers); ok {
		return ctx, func(flush func()) { flush() }
	}
	state := &diagnosticFinalizers{background: background}
	return context.WithValue(ctx, diagnosticFinalizersKey{}, state), func(flush func()) {
		state.mu.Lock()
		finish := state.finish
		state.finish = nil
		state.closed = true
		state.mu.Unlock()
		if state.background {
			flush()
			state.pending.Wait()
			return
		}
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
	if state.background {
		previous = state.previous
		state.previous = finish
		state.pending.Add(1)
		go func() {
			defer state.pending.Done()
			finish()
		}()
		return previous, true
	}
	if len(state.finish) > 0 {
		previous = state.finish[len(state.finish)-1]
	}
	state.finish = append(state.finish, finish)
	return previous, true
}

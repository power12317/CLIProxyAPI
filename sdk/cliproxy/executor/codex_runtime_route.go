package executor

import (
	"context"
	"sync/atomic"
)

type codexRuntimeRouteKey struct{}

// WithCodexRuntimeRoute keeps image retries on the selected runtime transport.
func WithCodexRuntimeRoute(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(codexRuntimeRouteKey{}).(*atomic.Bool); ok {
		return ctx
	}
	return context.WithValue(ctx, codexRuntimeRouteKey{}, &atomic.Bool{})
}

func RequireCodexRuntimeRoute(ctx context.Context) {
	if required, ok := ctx.Value(codexRuntimeRouteKey{}).(*atomic.Bool); ok {
		required.Store(true)
	}
}

func CodexRuntimeRouteRequired(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	required, _ := ctx.Value(codexRuntimeRouteKey{}).(*atomic.Bool)
	return required != nil && required.Load()
}

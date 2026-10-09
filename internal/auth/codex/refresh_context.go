package codex

import (
	"context"
	"errors"

	log "github.com/sirupsen/logrus"
)

type refreshContextKey struct{}

type refreshContext struct {
	credentialID string
	allowed      func() bool
}

// WithRefreshCredential carries the original CPA identity and the live refresh owner.
func WithRefreshCredential(ctx context.Context, credentialID string, allowed func() bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, refreshContextKey{}, refreshContext{credentialID, allowed})
}

func checkRefreshAllowed(ctx context.Context) error {
	state, _ := ctx.Value(refreshContextKey{}).(refreshContext)
	if state.allowed != nil && !state.allowed() {
		return errors.New("OAuth refresh is currently managed by Codex")
	}
	return nil
}

func refreshLogger(ctx context.Context) *log.Entry {
	state, _ := ctx.Value(refreshContextKey{}).(refreshContext)
	return log.WithFields(log.Fields{"provider": "codex", "auth_id": state.credentialID, "auth_file": state.credentialID, "operation": "oauth_refresh", "refresh_owner": "cpa"})
}

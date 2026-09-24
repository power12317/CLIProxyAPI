package executor

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
)

const CodexTransportMetadataKey = "codex_transport_state"
const CodexWebsocketMaxFailures = 5

// CodexTransportState belongs to a logical request, including auth and bootstrap retries.
// It must never be reused for the next request on a downstream connection.
type CodexTransportState struct {
	Failures     atomic.Int32
	HTTPFallback atomic.Bool
}

type codexTransportContextKey struct{}
type preferredUpstreamWebsocketKey struct{}
type upstreamTransportCallbackKey struct{}

func WithUpstreamTransportCallback(ctx context.Context, callback func(bool)) context.Context {
	return context.WithValue(ctx, upstreamTransportCallbackKey{}, callback)
}

func ReportUpstreamWebsocket(ctx context.Context, websocket bool) {
	if ctx == nil {
		return
	}
	if callback, ok := ctx.Value(upstreamTransportCallbackKey{}).(func(bool)); ok && callback != nil {
		callback(websocket)
	}
}

func EnsureCodexTransport(opts *Options) *CodexTransportState {
	if state, ok := opts.Metadata[CodexTransportMetadataKey].(*CodexTransportState); ok && state != nil {
		return state
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	state := &CodexTransportState{}
	metadata[CodexTransportMetadataKey] = state
	opts.Metadata = metadata
	return state
}

func WithCodexTransport(ctx context.Context, state *CodexTransportState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTransportContextKey{}, state)
}

func CodexTransport(ctx context.Context) *CodexTransportState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(codexTransportContextKey{}).(*CodexTransportState)
	return state
}

// WithPreferredUpstreamWebsocket changes resource ownership, not the downstream wire format.
func WithPreferredUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, preferredUpstreamWebsocketKey{}, true)
}

func PreferredUpstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	preferred, _ := ctx.Value(preferredUpstreamWebsocketKey{}).(bool)
	return preferred
}

func WebsocketExecutionSession(ctx context.Context) bool {
	return DownstreamWebsocket(ctx) || PreferredUpstreamWebsocket(ctx)
}

// ChatGPTCodexDestination deliberately does not rewrite custom provider endpoints.
func ChatGPTCodexDestination(provider, baseURL string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return false
	}
	if strings.TrimSpace(baseURL) == "" {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "chatgpt.com") &&
		(u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		strings.TrimSuffix(u.Path, "/") == "/backend-api/codex"
}

var ErrCodexWebsocketFallback = errors.New("codex websocket handshake failure budget exhausted")

// CodexReplayUnsafeError stops outer retries after an ambiguous send or stream disconnect.
// No HTTP status is fabricated: error presentation still comes from the underlying failure.
type CodexReplayUnsafeError struct{ Cause error }

func (e *CodexReplayUnsafeError) Error() string         { return e.Cause.Error() }
func (e *CodexReplayUnsafeError) Unwrap() error         { return e.Cause }
func (e *CodexReplayUnsafeError) IsRequestScoped() bool { return true }

func IsCodexReplayUnsafe(err error) bool {
	var unsafe *CodexReplayUnsafeError
	return errors.As(err, &unsafe)
}

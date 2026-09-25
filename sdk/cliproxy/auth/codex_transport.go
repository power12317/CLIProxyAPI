package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (m *Manager) prepareCodexTransport(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Options, func()) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || cfg.Codex.Basispoints.Enabled || !cfg.Codex.ForceWebsocket || opts.Alt == "responses/compact" || len(providers) != 1 || !strings.EqualFold(providers[0], "codex") {
		return ctx, opts, nil
	}
	cliproxyexecutor.EnsureCodexTransport(&opts)
	ctx = cliproxyexecutor.WithPreferredUpstreamWebsocket(ctx)
	if id, _ := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string); id != "" {
		return ctx, opts, nil
	}
	m.codexSessionPoolOnce.Do(func() { m.codexSessionPool = cliproxyexecutor.NewExecutionSessionPool(m.CloseExecutionSession) })
	id, release := m.codexSessionPool.Acquire(cliproxyexecutor.CodexSessionPoolKey(req, opts))
	opts.Metadata = cloneRequestMetadata(opts.Metadata)
	opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = id
	return ctx, opts, func() {
		if latest := m.runtimeConfigSnapshot(); latest == nil || !latest.Codex.ForceWebsocket {
			m.CloseExecutionSession(id)
		}
		release()
	}
}

func (m *Manager) drainCodexSessionPool() {
	m.codexSessionPoolOnce.Do(func() { m.codexSessionPool = cliproxyexecutor.NewExecutionSessionPool(m.CloseExecutionSession) })
	m.codexSessionPool.Drain()
}

func (m *Manager) codexWebsocketAuthEnabled(auth *Auth) bool {
	cfg := m.runtimeConfigSnapshot()
	if cfg != nil && cfg.Codex.Basispoints.Enabled && auth != nil && strings.EqualFold(auth.Provider, "codex") {
		return false
	}
	if authWebsocketsEnabled(auth) {
		return true
	}
	return cfg != nil && cfg.Codex.ForceWebsocket && auth != nil &&
		cliproxyexecutor.ChatGPTCodexDestination(auth.Provider, auth.Attributes["base_url"])
}

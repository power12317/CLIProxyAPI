package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexGlobalModeBlocksEveryRefreshEntry(t *testing.T) {
	for _, flag := range []string{"missing", "false", "true", "disabled"} {
		t.Run(flag, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}})
			executor := &countingRefreshExecutor{id: "codex"}
			m.RegisterExecutor(executor)
			metadata := map[string]any{"access_token": "still-usable", "refresh_token": "invalidated", "expired": "2000-01-01T00:00:00Z"}
			if flag != "missing" {
				metadata["codex_cli"] = map[string]any{"enabled": flag != "false"}
			}
			metadata["disabled"] = flag == "disabled"
			auth := &Auth{ID: "original.json", Provider: "codex", Disabled: flag == "disabled", Metadata: metadata}
			if _, err := m.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			if _, scheduled := m.nextRefreshCheckAt(time.Now(), auth, time.Minute); scheduled || m.shouldRefresh(auth, time.Now()) {
				t.Fatal("global Codex mode scheduled a local refresh")
			}
			m.refreshAuth(ctx, auth.ID)
			if _, err := m.ForceRefreshAuth(ctx, auth.ID); err == nil {
				t.Fatal("manual refresh was accepted")
			}
			if results := m.ForceRefreshAll(ctx); len(results) != 0 {
				t.Fatal("batch refresh included a Codex credential")
			}
			if _, refreshed := m.tryRefreshAfterUnauthorized(ctx, auth, oauthStatusError{code: http.StatusUnauthorized}, false); refreshed {
				t.Fatal("401 handling attempted refresh")
			}
			if _, err := m.refreshAuthForRequest(ctx, auth.ID, "still-usable"); err == nil {
				t.Fatal("direct request refresh was accepted")
			}
			current, _ := m.GetByID(auth.ID)
			if executor.refreshCalls.Load() != 0 || current.Metadata["access_token"] != "still-usable" || current.RejectedAccessToken != "" {
				t.Fatal("blocked refresh altered or refreshed the credential")
			}
			// Turning the global mode off permits refresh even with a stale true file flag.
			m.SetConfig(&config.Config{})
			if _, err := m.ForceRefreshAuth(ctx, auth.ID); err != nil {
				t.Fatal(err)
			}
			if executor.refreshCalls.Load() != 1 {
				t.Fatal("CPA did not regain refresh ownership")
			}
		})
	}
}

func TestCodexModeChangeRechecksQueuedRefreshAndReschedules(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	executor := &countingRefreshExecutor{id: "codex"}
	m.RegisterExecutor(executor)
	auth, err := m.Register(ctx, &Auth{ID: "queued.json", Provider: "codex", Metadata: map[string]any{"refresh_token": "refresh", "expired": "2000-01-01T00:00:00Z", "refresh_interval_seconds": 60}})
	if err != nil {
		t.Fatal(err)
	}
	loop := newAuthAutoRefreshLoop(m, time.Minute, 1)
	m.refreshLoop = loop
	now := time.Now()
	job := m.markRefreshPending(loop, auth.ID, auth.RegistrationEpoch, now)
	if job == nil {
		t.Fatal("refresh job was not queued")
	}
	m.SetConfig(&config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}})
	if m.beginRefreshJob(ctx, job) {
		t.Fatal("old queued job started after mode changed")
	}
	m.finishRefreshJob(job, time.Time{}, false)
	loop.applyDirty(now)
	if _, ok := loop.peek(); ok {
		t.Fatal("Codex credential remained in the refresh schedule")
	}
	m.SetConfig(&config.Config{})
	loop.applyDirty(now)
	if _, ok := loop.peek(); !ok {
		t.Fatal("turning mode off did not reschedule the credential")
	}
}

func TestInvalidatedRefreshKeepsUnexpiredCodexAccessToken(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &mockOAuthErrorExecutor{id: "codex", errToReturn: oauthStatusError{code: 401, msg: `{"error":{"code":"refresh_token_invalidated"}}`}}
	m.RegisterExecutor(executor)
	auth := &Auth{ID: "usable.json", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "usable-access", "refresh_token": "invalid-refresh", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ForceRefreshAuth(context.Background(), auth.ID); err == nil {
		t.Fatal("expected a refresh error")
	}
	current, _ := m.GetByID(auth.ID)
	if current.Metadata["access_token"] != "usable-access" || current.Disabled || current.Unavailable || current.Status != StatusActive {
		t.Fatal("refresh failure invalidated the usable access token")
	}
}

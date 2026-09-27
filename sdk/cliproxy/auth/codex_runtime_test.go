package auth

import (
	"context"
	"testing"
	"time"
)

func TestCodexRuntimeNeverRefreshesOrPersistsTokens(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{}
	m := NewManager(store, nil, nil)
	executor := &countingRefreshExecutor{id: CodexRuntimeProvider}
	m.RegisterExecutor(executor)
	a := &Auth{ID: "codex-runtime:worker", Provider: CodexRuntimeProvider, Metadata: map[string]any{"access_token": "synthetic", "refresh_token": "synthetic", "expired": "2000-01-01T00:00:00Z"}}
	if _, err := m.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, scheduled := nextRefreshCheckAt(time.Now(), a, time.Minute); scheduled || m.shouldRefresh(a, time.Now()) || authHasRefreshCredential(a) {
		t.Fatal("runtime credential entered CPA refresh scheduling")
	}
	if _, err := m.ForceRefreshAuth(ctx, a.ID); err == nil {
		t.Fatal("forced refresh was accepted")
	}
	if got := m.ForceRefreshAll(ctx); len(got) != 0 {
		t.Fatal("batch refresh included runtime credential")
	}
	if _, err := m.refreshAuthForRequest(ctx, a.ID, "synthetic"); err == nil {
		t.Fatal("request refresh was accepted")
	}
	m.MarkResult(ctx, Result{AuthID: a.ID, Provider: CodexRuntimeProvider, Model: "model", Success: true})
	if executor.refreshCalls.Load() != 0 || store.saveCount.Load() != 0 {
		t.Fatal("CPA accessed runtime credential persistence or refresh")
	}
}

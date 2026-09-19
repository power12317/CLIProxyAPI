package helps

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateCachesAndReplaysResponseHeader(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-1", Metadata: map[string]any{"email": "user@example.com"}}
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-1\",\"session_id\":\"session-1\"}"}}`)
	first := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	first.ObserveResponse(&http.Response{
		Request: &http.Request{URL: mustURL(t, "https://chatgpt.com/backend-api/codex/responses")},
		Header:  http.Header{"X-Codex-Turn-State": {"ts-1"}},
	})

	second := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	headers := make(http.Header)
	second.ApplyHeaders(headers)
	if got := headers.Get(codexTurnStateHeader); got != "ts-1" {
		t.Fatalf("replayed turn state = %q, want ts-1", got)
	}
	if second.sessionID != "session-1" || second.key.turnID != "turn-1" {
		t.Fatalf("request identity = session %q, turn %q", second.sessionID, second.key.turnID)
	}
}

func TestCodexTurnStateRefreshesTTLWithoutReadRenewal(t *testing.T) {
	now := time.Unix(200, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	body := turnStateBody("turn-1")
	state := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	state.observe("ts-1")
	initial := state.bucket.entries[state.key].expiresAt

	now = now.Add(30 * time.Minute)
	reader := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	if _, ok := reader.cached(); !ok {
		t.Fatal("expected cached state before TTL expiry")
	}
	if got := state.bucket.entries[state.key].expiresAt; !got.Equal(initial) {
		t.Fatalf("read renewed TTL to %v, want unchanged %v", got, initial)
	}

	state.observe("ts-2")
	refreshed := state.bucket.entries[state.key].expiresAt
	if !refreshed.Equal(now.Add(codexTurnStateTTL)) {
		t.Fatalf("refreshed expiry = %v, want %v", refreshed, now.Add(codexTurnStateTTL))
	}
	if got := state.bucket.entries[state.key].state; got != "ts-2" {
		t.Fatalf("cached state = %q, want ts-2", got)
	}
}

func TestCodexTurnStateExpiresAndIsolated(t *testing.T) {
	now := time.Unix(300, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth1 := &cliproxyauth.Auth{ID: "auth-1"}
	auth2 := &cliproxyauth.Auth{ID: "auth-2"}
	state := cache.request(auth1, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	state.observe("ts-1")

	otherTurn := cache.request(auth1, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-2"), nil)
	otherAuth := cache.request(auth2, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	otherOrigin := cache.request(auth1, "https://proxy.example/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	for name, candidate := range map[string]*CodexTurnState{"other turn": otherTurn, "other auth": otherAuth, "other origin": otherOrigin} {
		headers := make(http.Header)
		candidate.ApplyHeaders(headers)
		if got := headers.Get(codexTurnStateHeader); got != "" {
			t.Errorf("%s received cached state %q", name, got)
		}
	}

	now = now.Add(codexTurnStateTTL)
	reader := cache.request(auth1, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	if _, ok := reader.cached(); ok {
		t.Fatal("expired turn state was still readable")
	}
}

func TestCodexTurnStateObservesWebsocketMetadataAndMirrorsBody(t *testing.T) {
	now := time.Unix(400, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	body := turnStateBody("turn-1")
	state := cache.request(auth, "wss://chatgpt.com/backend-api/codex/responses", body, nil)
	state.observe("ts-1")

	updated := state.ApplyWebsocketBody([]byte(`{"type":"response.create","client_metadata":{"turn_id":"turn-1"}}`))
	if got := gjson.GetBytes(updated, "client_metadata.x-codex-turn-state").String(); got != "ts-1" {
		t.Fatalf("websocket body state = %q, want ts-1", got)
	}

	state.ObserveEvent([]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"ts-2"}}`))
	reader := cache.request(auth, "wss://chatgpt.com/backend-api/codex/responses", body, nil)
	headers := make(http.Header)
	reader.ApplyHeaders(headers)
	if got := headers.Get(codexTurnStateHeader); got != "ts-2" {
		t.Fatalf("metadata state = %q, want ts-2", got)
	}
	if state.responseLen != len("ts-2") {
		t.Fatalf("response state length = %d, want %d", state.responseLen, len("ts-2"))
	}
}

func TestCodexTurnStateIgnoresMissingTurnAndEmptyResponses(t *testing.T) {
	now := time.Unix(500, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	missing := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", []byte(`{"client_metadata":{}}`), nil)
	if missing.bucket != nil {
		t.Fatal("missing turn unexpectedly allocated a cache bucket")
	}
	headers := make(http.Header)
	missing.ApplyHeaders(headers)
	if got := headers.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("missing turn received state %q", got)
	}
	missing.ObserveResponse(&http.Response{Header: http.Header{codexTurnStateHeader: {""}}})
	if missing.responseLen != 0 {
		t.Fatalf("empty response length = %d, want 0", missing.responseLen)
	}

	state := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	state.observe("ts-1")
	state.ObserveResponse(&http.Response{Header: make(http.Header)})
	reader := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	if got, ok := reader.cached(); !ok || got != "ts-1" {
		t.Fatalf("empty response cleared state: %q, %v", got, ok)
	}
}

func TestCodexTurnStateAuthHeaderOverride(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-override-test", Attributes: map[string]string{"header:X-Codex-Turn-State": "configured"}}
	state := NewCodexTurnState(context.Background(), auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil, "", nil)
	headers := make(http.Header)
	state.ApplyHeaders(headers)
	if got := headers.Get(codexTurnStateHeader); got != "configured" {
		t.Fatalf("configured state = %q, want configured", got)
	}
}

func TestCodexTurnStateInvalidationDropsCredentialBucket(t *testing.T) {
	now := time.Unix(600, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	body := turnStateBody("turn-1")
	state := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	state.observe("ts-1")
	cache.invalidate(auth.ID)

	reader := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", body, nil)
	headers := make(http.Header)
	reader.ApplyHeaders(headers)
	if got := headers.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("invalidated credential received state %q", got)
	}
}

func turnStateBody(turnID string) []byte {
	return []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"` + turnID + `\"}"}}`)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}

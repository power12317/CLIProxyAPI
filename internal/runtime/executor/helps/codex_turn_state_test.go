package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateLogUpdatesPreserveResponseModelAndResetAttempts(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", FileName: "codex-user-windows.json"}
	t.Cleanup(func() { InvalidateCodexTurnStates(auth.ID) })
	const requested = "gpt-6-sol"

	for _, served := range []string{"gpt-5.6-luna", "", requested} {
		// Reuse identical identity and model to exercise retries on one Gin context.
		state := NewCodexTurnState(ctx, auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil, requested, nil)
		fields, ok := logging.CodexTurnStateLogFieldsForContext(ginCtx)
		if !ok || fields.ResponseModel != "" {
			t.Fatalf("new attempt inherited response model: %+v", fields)
		}
		state.ObserveRequest(http.Header{codexTurnStateHeader: {"request-state"}}, nil)
		reporter := newCodexTestReporter(ctx, requested, auth)
		if served != "" {
			reporter.ObserveCodexResponseModel([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"model":%q}}`, served)))
		}
		reporter.EnsurePublished(ctx)
		state.ObserveEvent([]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"response-state"}}`))
		state.LogResponse(ctx, nil, true)
		fields, ok = logging.CodexTurnStateLogFieldsForContext(ginCtx)
		if !ok || fields.RequestedModel != requested || fields.ResponseModel != served {
			t.Fatalf("final model pair = %q/%q, want %q/%q", fields.RequestedModel, fields.ResponseModel, requested, served)
		}
		if fields.RequestTurnStateLen != len("request-state") || fields.ResponseTurnStateLen != len("response-state") {
			t.Fatalf("final turn-state lengths lost: %+v", fields)
		}
	}
}

func TestCodexTurnStatePrefersStandardMetadataAcrossRequests(t *testing.T) {
	for _, firstValue := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_value=%t", firstValue), func(t *testing.T) {
			cache := newCodexTurnStateCache(func() time.Time { return time.Unix(100, 0) })
			auth := &cliproxyauth.Auth{ID: "metadata-priority", Metadata: map[string]any{"account_id": "owner-1"}}
			const target = "wss://chatgpt.com/backend-api/codex/responses"
			body := turnStateBody("turn-1")
			for _, step := range []struct{ kind, value, want string }{
				{"codex.response.metadata", "fallback-first", "fallback-first"},
				{"response.metadata", "", "fallback-first"},
				{"response.metadata", "standard-first", "standard-first"},
				{"codex.response.metadata", "fallback-later", "standard-first"},
			} {
				state := cache.request(auth, target, body, nil)
				state.firstValue = firstValue
				state.ObserveEvent([]byte(fmt.Sprintf(`{"type":%q,"headers":{"X-Codex-Turn-State":%q}}`, step.kind, step.value)))
				reader := cache.request(auth, target, body, nil)
				if got := gjson.GetBytes(reader.ApplyWebsocketBody(body), "client_metadata.x-codex-turn-state").String(); got != step.want {
					t.Fatalf("after %s=%q: state=%q, want %q", step.kind, step.value, got, step.want)
				}
			}
			state := cache.request(auth, target, body, nil)
			state.firstValue = firstValue
			state.ObserveEvent([]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"standard-later"}}`))
			want := "standard-later"
			if firstValue {
				want = "standard-first"
			}
			if got, _ := state.cached(); got != want {
				t.Fatalf("same-source retention=%q, want %q", got, want)
			}
			if got, exists := cache.request(auth, target, turnStateBody("turn-2"), nil).cached(); exists {
				t.Fatalf("new turn inherited %q", got)
			}
			auth.Metadata["account_id"] = "owner-2"
			if got, exists := cache.request(auth, target, body, nil).cached(); exists {
				t.Fatalf("new owner inherited %q", got)
			}
			state.ObserveEvent([]byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"late-old-owner"}}`))
			if _, exists := cache.request(auth, target, body, nil).cached(); exists {
				t.Fatal("late old-owner event resurrected state")
			}
		})
	}
}

func TestCodexTurnStateFrameIdentityOverridesStaleHandshake(t *testing.T) {
	cache := newCodexTurnStateCache(time.Now)
	auth := &cliproxyauth.Auth{ID: t.Name()}
	headers := http.Header{"X-Codex-Turn-Metadata": {`{"turn_id":"old-turn"}`}}
	for _, body := range []string{
		`{"client_metadata":{"turn_id":""}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"\"}"}}`,
	} {
		state := cache.requestWithIdentity(auth, "wss://chatgpt.com", []byte(body), headers, true)
		if state.key.turnID != "" || state.bucket != nil {
			t.Fatalf("prewarm inherited handshake turn: %q", state.key.turnID)
		}
	}
}

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

func TestCodexTurnStateObserveRequestOnlyChangesStatistics(t *testing.T) {
	now := time.Unix(700, 0)
	cache := newCodexTurnStateCache(func() time.Time { return now })
	auth := &cliproxyauth.Auth{ID: "auth-final-length"}
	state := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("turn-1"), nil)
	state.observe(strings.Repeat("p", 312))
	initial := state.bucket.entries[state.key]

	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
		want    int
	}{
		{name: "final header", headers: http.Header{"x-codex-turn-state": {strings.Repeat("h", 292)}}, want: 292},
		{name: "websocket frame overrides handshake", headers: http.Header{codexTurnStateHeader: {strings.Repeat("h", 292)}}, body: `{"client_metadata":{"x-codex-turn-state":"` + strings.Repeat("b", 332) + `"}}`, want: 332},
		{name: "explicit empty frame", headers: http.Header{codexTurnStateHeader: {strings.Repeat("h", 292)}}, body: `{"client_metadata":{"x-codex-turn-state":""}}`, want: 0},
		{name: "absent state clears old length", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.ObserveRequest(tc.headers, []byte(tc.body))
			requestLen, responseLen := state.turnStateLengths()
			if requestLen != tc.want || responseLen != 312 {
				t.Fatalf("lengths = %d/%d, want %d/312", requestLen, responseLen, tc.want)
			}
			if current := state.bucket.entries[state.key]; current != initial {
				t.Fatal("recording the request modified the passive cache or its expiry")
			}
		})
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

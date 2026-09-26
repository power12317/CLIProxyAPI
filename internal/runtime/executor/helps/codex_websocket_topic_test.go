package helps

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexWebsocketTopicIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		headers          http.Header
	}{
		{"frame thread before session/cache", `{"client_metadata":{"thread_id":"child","session_id":"parent"},"prompt_cache_key":"shared"}`, "child", nil},
		{"frame before stale handshake", `{"client_metadata":{"thread_id":"child"}}`, "child", http.Header{"Thread-Id": {"parent"}}},
		{"nested frame metadata", `{"client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"child\"}"}}`, "child", nil},
		{"header aliases", `{}`, "thread", http.Header{"thread_id": {"thread"}, "session-id": {"parent"}}},
		{"header metadata", `{}`, "thread", http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"thread","session_id":"parent"}`}}},
		{"top thread", `{"thread_id":"thread"}`, "thread", nil},
		{"session fallback", `{"client_metadata":{"session_id":"session"},"prompt_cache_key":"cache"}`, "session", nil},
		{"cache fallback", `{"prompt_cache_key":"cache"}`, "cache", nil},
		{"turn alone is not topic", `{"client_metadata":{"turn_id":"turn"}}`, "", nil},
		{"child must not use parent cache", `{"prompt_cache_key":"parent"}`, "", http.Header{"X-Codex-Parent-Thread-Id": {"parent"}}},
		{"child session distinct from parent", `{"client_metadata":{"session_id":"child"}}`, "child", http.Header{"X-Codex-Parent-Thread-Id": {"parent"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexWebsocketTopic([]byte(tc.body), tc.headers); got != tc.want {
				t.Fatalf("topic=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestCodexWebsocketTopicOwnership(t *testing.T) {
	credential := &auth.Auth{ID: "credential", Provider: "codex"}
	req := core.Request{Model: "model-a", Payload: []byte(`{"client_metadata":{"thread_id":"topic","turn_id":"turn-a"}}`)}
	opts := core.Options{Metadata: map[string]any{core.CallerScopeMetadataKey: "caller", core.ExecutionSessionMetadataKey: "connection-a"}}
	first := CodexWebsocketTopicSessionID(credential, req, opts, "wss://upstream/responses")
	req.Model = "model-b"
	req.Payload = []byte(`{"client_metadata":{"thread_id":"topic","turn_id":"turn-b"}}`)
	opts.Metadata[core.ExecutionSessionMetadataKey] = "connection-b"
	if got := CodexWebsocketTopicSessionID(credential, req, opts, "wss://upstream/responses"); got != first {
		t.Fatal("turn/model/downstream reconnect split the topic")
	}
	opts.Headers = http.Header{"Thread-Id": {"topic"}}
	req.Payload = []byte(`{}`)
	if got := CodexWebsocketTopicSessionID(credential, req, opts, "wss://upstream/responses"); got != first {
		t.Fatal("header and frame topic identities differ")
	}
	opts.Metadata[core.CallerScopeMetadataKey] = "other-caller"
	if got := CodexWebsocketTopicSessionID(credential, req, opts, "wss://upstream/responses"); got == first {
		t.Fatal("caller isolation lost")
	}
	opts.Metadata[core.CallerScopeMetadataKey] = "caller"
	credential.ID = "other-credential"
	if got := CodexWebsocketTopicSessionID(credential, req, opts, "wss://upstream/responses"); got == first {
		t.Fatal("credential isolation lost")
	}
}

func TestCodexTopicWaitsForResponseAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var activity CodexWebsocketActivity
		if err := activity.Acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		activity.Sent([]byte(`{"type":"response.create"}`))
		activity.Received([]byte(`{"type":"response.created","response":{"id":"response-a"}}`))
		activity.Release()
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		go func() { done <- activity.Acquire(ctx) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("borrowed a socket with an unfinished response")
		default:
		}
		cancel()
		if err := <-done; err != context.Canceled {
			t.Fatalf("wait cancellation=%v", err)
		}
		go func() { done <- activity.Acquire(t.Context()) }()
		activity.Received([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"state"}}`))
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("metadata ended the response")
		default:
		}
		activity.Received([]byte(`{"type":"response.completed","response":{"id":"response-a","output":[]}}`))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		activity.Release()
	})
}

func TestCodexTopicSteeringBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var activity CodexWebsocketActivity
		if err := activity.Acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		frames := []struct {
			send bool
			body string
		}{
			{true, `{"type":"response.create"}`},
			{false, `{"type":"response.created","response":{"id":"parent"}}`},
			{true, `{"type":"response.steer","previous_response_id":"parent"}`},
			{false, `{"type":"response.steer.accepted","steer":{"id":"steer","previous_response_id":"parent"}}`},
			{false, `{"type":"response.completed","response":{"id":"parent"}}`},
		}
		for _, f := range frames {
			if f.send {
				activity.Sent([]byte(f.body))
			} else {
				activity.Received([]byte(f.body))
			}
		}
		activity.Release()
		done := make(chan error, 1)
		go func() { done <- activity.Acquire(t.Context()) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("accepted successor was discarded")
		default:
		}
		activity.Received([]byte(`{"type":"response.steer.pending","reason":"waiting_for_required_input","steer":{"id":"steer","previous_response_id":"parent"}}`))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		activity.Sent([]byte(`{"type":"response.create","previous_response_id":"parent"}`))
		activity.Received([]byte(`{"type":"response.created","response":{"id":"child"}}`))
		activity.Received([]byte(`{"type":"response.completed","response":{"id":"child"}}`))
		activity.Release()
		if err := activity.Acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		activity.Release()
	})
}

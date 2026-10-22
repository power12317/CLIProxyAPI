package session

import (
	"net/http"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestClientMetadataKeepsSessionAcrossBusinessChanges(t *testing.T) {
	for _, body := range []string{
		`{"input":"first","instructions":"a","client_metadata":{"session_id":"source-session","thread_id":"source-thread"}}`,
		`{"input":"next","instructions":"b","tools":[],"client_metadata":{"session_id":"source-session","thread_id":"source-thread"}}`,
		`{"input":"compressed","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"source-session\",\"thread_id\":\"source-thread\",\"window_id\":\"window-2\"}"}}`,
	} {
		req, opts := Enrich(coreexecutor.Request{Payload: []byte(body)}, coreexecutor.Options{})
		info, ok := ExtractSessionInfo(nil, []byte(body), nil)
		if !ok || info.SessionID == "" {
			t.Fatalf("missing session for %s", body)
		}
		want := BoundSessionIdentity(info.SessionID)
		if req.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != want || opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != want {
			t.Fatalf("session extraction was not used: %v", opts.Metadata)
		}
		if info.SessionID != "codex:source-thread" {
			t.Fatalf("unexpected logical thread: %+v", info)
		}
	}
	info, ok := ExtractSessionInfo(http.Header{"Session-Id": {"header-session"}, "Thread-Id": {"header-thread"}}, []byte(`{"client_metadata":{"session_id":"body-session","thread_id":"body-thread"}}`), nil)
	if !ok || info.SessionID != "codex:header-thread" {
		t.Fatalf("header precedence changed: %+v", info)
	}
}

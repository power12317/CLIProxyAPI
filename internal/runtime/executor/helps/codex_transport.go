package helps

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// A warmup that cannot open WS must not become a billable HTTP generation.
func CodexWebsocketPrewarmFallback(ctx context.Context, req cliproxyexecutor.Request) *cliproxyexecutor.StreamResult {
	generate := gjson.GetBytes(req.Payload, "generate")
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || !generate.Exists() || generate.Bool() {
		return nil
	}
	id := "resp_cpa_warmup_" + uuid.NewString()
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	for _, kind := range []string{"response.created", "response.completed"} {
		status := "in_progress"
		if kind == "response.completed" {
			status = "completed"
		}
		body, _ := json.Marshal(map[string]any{"type": kind, "response": map[string]any{"id": id, "object": "response", "model": req.Model, "status": status, "output": []any{}}})
		chunks <- cliproxyexecutor.StreamChunk{Payload: body}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}
}

// GuardCodexWebsocketFailure prevents credential and bootstrap layers from replaying
// a request whose transmission or execution status is ambiguous.
func GuardCodexWebsocketFailure(ctx context.Context, err error) error {
	if err == nil || cliproxyexecutor.CodexTransport(ctx) == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var scoped cliproxyexecutor.RequestScopedError
	if errors.As(err, &scoped) && scoped.IsRequestScoped() {
		return err
	}
	return &cliproxyexecutor.CodexReplayUnsafeError{Cause: err}
}

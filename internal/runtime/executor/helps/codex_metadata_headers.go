package helps

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// CodexTurnMetadataHeader leaves the unbounded tool inventory in the body,
// matching the native client's compact compatibility header representation.
func CodexTurnMetadataHeader(raw string) string {
	var metadata map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata == nil {
		return raw
	}
	delete(metadata, "tool_namespaces_info")
	encoded, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return raw
	}
	return string(encoded)
}

// RestoreCodexMetadataHeaders restores only documented body/header mirrors.
// Existing client headers and configured overrides take precedence.
func RestoreCodexMetadataHeaders(headers http.Header, body []byte) {
	if headers == nil {
		return
	}
	for _, key := range []string{"X-Codex-Parent-Thread-Id", "X-OpenAI-Subagent", "X-Codex-Window-Id", "X-Codex-Turn-Metadata"} {
		if headers.Get(key) != "" {
			continue
		}
		value := gjson.GetBytes(body, "client_metadata."+strings.ToLower(key))
		if value.Type != gjson.String || value.String() == "" {
			continue
		}
		raw := value.String()
		if key == "X-Codex-Turn-Metadata" {
			raw = CodexTurnMetadataHeader(raw)
		}
		headers.Set(key, raw)
	}
}

// CodexWebsocketClientHeaders resolves the same inbound header source used by
// the existing handshake pipeline. Callers must not mutate the returned map.
func CodexWebsocketClientHeaders(ctx context.Context, explicit http.Header) http.Header {
	if explicit != nil {
		return explicit
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			return ginCtx.Request.Header
		}
	}
	return nil
}

// RestoreCodexWebsocketIdentityHeaders preserves explicit client identities and
// fills missing headers from the existing preparation/fidelity results. It does
// not generate IDs or change the body, execution metadata, or topic ownership.
// Configured header overrides and identity-confusion policy run afterwards.
func RestoreCodexWebsocketIdentityHeaders(headers, source http.Header, body []byte) {
	if headers == nil {
		return
	}
	root := gjson.ParseBytes(body)
	metadata := gjson.Parse(root.Get("client_metadata.x-codex-turn-metadata").String())
	sessionID := firstString(
		topicHeader(source, "Session-Id", "Session_id"),
		topicHeader(headers, "Session-Id", "Session_id"),
		root.Get("prompt_cache_key").String(),
		root.Get("client_metadata.session_id").String(), metadata.Get("session_id").String(), root.Get("session_id").String(),
		root.Get("client_metadata.thread_id").String(), metadata.Get("thread_id").String(), root.Get("thread_id").String(),
		topicHeader(source, "Thread-Id", "Thread_id"),
	)
	threadID := firstString(
		topicHeader(source, "Thread-Id", "Thread_id"), topicHeader(headers, "Thread-Id", "Thread_id"),
		root.Get("client_metadata.thread_id").String(), metadata.Get("thread_id").String(), root.Get("thread_id").String(), sessionID,
	)
	requestID := firstString(topicHeader(source, "X-Client-Request-Id"), threadID, topicHeader(headers, "X-Client-Request-Id"))
	for key, value := range map[string]string{"Session-Id": sessionID, "Thread-Id": threadID, "X-Client-Request-Id": requestID} {
		if value == "" {
			continue
		}
		for existing := range headers {
			if strings.EqualFold(existing, key) || key == "Session-Id" && strings.EqualFold(existing, "session_id") || key == "Thread-Id" && strings.EqualFold(existing, "thread_id") {
				delete(headers, existing)
			}
		}
		headers.Set(key, value)
	}
}

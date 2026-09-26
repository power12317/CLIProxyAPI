package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const CodexTopicSessionPrefix = "codex-topic/"

// CodexWebsocketTopic resolves native thread identity before cache affinity.
// Per-frame metadata takes precedence over an older downstream handshake.
// Parent IDs and cache keys must never merge distinct child threads.
func CodexWebsocketTopic(payload []byte, headers http.Header) string {
	root := gjson.ParseBytes(payload)
	metadata := gjson.Parse(root.Get("client_metadata.x-codex-turn-metadata").String())
	headerMetadata := gjson.Parse(topicHeader(headers, "X-Codex-Turn-Metadata"))
	first := func(values ...string) string {
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
		return ""
	}
	thread := first(root.Get("client_metadata.thread_id").String(), metadata.Get("thread_id").String(),
		root.Get("thread_id").String(), root.Get("threadId").String(), root.Get("metadata.thread_id").String(),
		topicHeader(headers, "Thread-Id", "Thread_id", "X-Thread-Id"), headerMetadata.Get("thread_id").String())
	if thread != "" {
		return thread
	}
	session := first(root.Get("client_metadata.session_id").String(), metadata.Get("session_id").String(),
		root.Get("session_id").String(), root.Get("sessionId").String(), root.Get("metadata.session_id").String(),
		topicHeader(headers, "Session-Id", "Session_id", "X-Session-ID"), headerMetadata.Get("session_id").String())
	parent := first(root.Get("client_metadata.parent_thread_id").String(), metadata.Get("parent_thread_id").String(),
		topicHeader(headers, "X-Codex-Parent-Thread-Id"), headerMetadata.Get("parent_thread_id").String())
	child := first(root.Get("client_metadata.x-openai-subagent").String(), topicHeader(headers, "X-OpenAI-Subagent"))
	if parent != "" || child != "" && child != "false" && child != "0" {
		if session != "" && session != parent {
			return session
		}
		// A parent/cache identity alone does not identify a child. Let the caller
		// keep its existing execution session instead of borrowing the parent's.
		return ""
	}
	return first(session, root.Get("prompt_cache_key").String(), root.Get("promptCacheKey").String())
}

func topicHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		for key, values := range headers {
			if strings.EqualFold(key, name) {
				for _, value := range values {
					if value = strings.TrimSpace(value); value != "" {
						return value
					}
				}
			}
		}
	}
	return ""
}

// CodexWebsocketTopicSessionID is an internal lookup key only. It is never
// inserted into upstream JSON or headers. Models and turns do not own sockets.
func CodexWebsocketTopicSessionID(auth *cliproxyauth.Auth, req core.Request, opts core.Options, upstreamURL string) string {
	payload := opts.OriginalRequest
	if len(payload) == 0 {
		payload = req.Payload
	}
	topic := CodexWebsocketTopic(payload, opts.Headers)
	if topic == "" || auth == nil {
		return ""
	}
	scope := metadataString(opts.Metadata, core.CallerScopeMetadataKey)
	if scope == "" {
		scope = metadataString(req.Metadata, core.CallerScopeMetadataKey)
	}
	credential := auth.ID
	if credential == "" {
		credential = CodexOAuthAccessToken(auth)
		if credential == "" {
			credential = auth.Attributes["api_key"]
		}
	}
	encoded, _ := json.Marshal([]string{scope, credential, CodexOwnerFingerprint(auth), upstreamURL, topic})
	digest := sha256.Sum256(encoded)
	return CodexTopicSessionPrefix + hex.EncodeToString(digest[:])
}

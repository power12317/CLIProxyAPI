# Basispoints native request and WebSocket transport

The protocol was checked against the public extension build `1790362980` at
`https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/`.
Its main asset is `x-square-DUrhLSGN.js`, SHA-256
`7dbf4d37099e9040b07d063cf9554d8b1ada166bf0874aeda1e194f3bb9d851b`.

## Request metadata

- Both transports send `X-OpenAI-Account-User-Id` when the selected access token
  contains `https://api.openai.com/auth.chatgpt_account_user_id`. No value is
  synthesized when the claim is absent.
- `metadata.bps_tools_version_id` defaults to
  `tools-excel-core-2026-06-16-3af59f22`. An explicit client version is preserved.
- `agent_iteration` advances once for a batch of tool results, rather than once
  for every parallel tool result. It is derived from history, so retransmitting
  the same request or rotating accounts does not increment it again.
- Task/turn IDs, the reasoning pipeline, original model names, and the existing
  `context_management`, `prompt_cache_key`, and `service_tier` behavior are unchanged.

## Transport selection

With Basispoints enabled, either `codex.force-websocket: true` or a downstream
WebSocket request selects the native Basispoints WebSocket transport. Ordinary
HTTP clients continue to use SSE upstream. Streaming and aggregated responses
share the existing Basispoints tool conversion and usage pipeline.

The handshake targets `wss://bps.openai.com/basispoints/api/responses` with
`bps_client_info`, `bps_auth_mode=chatgpt`, a request-local `bps_ws_affinity`, and
the native control-frame negotiation. Authentication uses the `responses` and
`openai-bearer.<access-token>` subprotocols, together with the selected account
headers. Request, credential, and global proxy precedence use the existing dialer.

The generation frame contains the prepared Basispoints body plus
`type=response.create`, a UUID `basispoints_request_id`, and a random
`resume_<hex>` capability. The bearer subprotocol and resume capability are
redacted in request logs. The original client's 16 MiB outgoing frame limit is
honored by selecting SSE for larger requests, without truncating their contents.

Connections are scoped to a generation. A disconnect resumes the same generation
with `basispoints.response.resume`, its latest resume token, and `after_cursor`.
The affinity remains stable during these reconnects. Replay cursors suppress
duplicate events. Recovery attempts are bounded at three reconnects; a failed
in-flight recovery is returned to the client instead of restarting generation.
Cancellation closes the active connection and releases the reader. No generation
read/write deadlines or background reconnects are introduced.

Before a generation frame has been sent, five failed handshakes trigger SSE
fallback for that request. The next request tries WebSocket again. Authentication,
policy, validation, and rate-limit errors are returned directly; HTTP 404, 405,
and 426 remain eligible for transport fallback. A 403 during handshake or in a
stream enters the existing 30-minute Basispoints cooldown. A manual disable
continues to take precedence over automatic recovery.

The downstream history bridge remains enabled, including materialization of
`previous_response_id`. No connection or account is pinned across generations.
Native hosted-tool routing and the native Codex transport remain separate.

## Validation

Local WebSocket servers cover the routing matrix, native authentication and
request frames, streaming and non-streaming tool conversion, resume tokens and
cursors, deduplication, cancellation, handshake fallback, 403 cooldown, and full
error logs with request logging both enabled and disabled. Existing Basispoints
tests cover optional fields, model/effort passthrough, account rotation, encrypted
tool semantics, and turn identity. These are local protocol tests, not claims of
successful generation against an authenticated production Basispoints account.

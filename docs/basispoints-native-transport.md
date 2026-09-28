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
`bps_client_info`, `bps_auth_mode=chatgpt`, a session-owned `bps_ws_affinity`, and
the native control-frame negotiation. Authentication uses the `responses` and
`openai-bearer.<access-token>` subprotocols, together with the selected account
headers. Request, credential, and global proxy precedence use the existing dialer.

The generation frame contains the prepared Basispoints body plus
`type=response.create`, a UUID `basispoints_request_id`, and a random
`resume_<hex>` capability. The bearer subprotocol and resume capability are
redacted in request logs. The original client's 16 MiB outgoing frame limit is
honored by selecting SSE for larger requests, without truncating their contents.

Connections belong to the existing full `session_id`, rather than a request,
turn, or model. A completed response releases request ownership without closing
the connection. The shared store survives executor replacement and config reload;
concurrent requests for the same selected connection use the existing Codex
request-ownership gate. Idle reads process ping/pong and detect peer disconnects.
No idle expiry or proactive reconnect is introduced. After an idle disconnect,
the next request opens a connection on demand.

A disconnect during generation resumes that generation with
`basispoints.response.resume`, its latest resume token, and `after_cursor`.
The affinity remains stable during these reconnects. Replay cursors and request
IDs are reset for each new response. Recovery attempts are bounded at three
reconnects; a failed in-flight recovery is returned to the client instead of
restarting generation. Cancellation before completion still closes the active
connection, while cancelling an already completed request cannot close the
retained session connection. No generation read/write deadlines are added.

An initial HTTP-400 error event with code
`websocket_connection_limit_reached` retires the expired connection and resends
the full request once on a new WebSocket, even when `request-retry` is zero.
The request body, session, turn and selected credential stay unchanged; request
and resume identities are renewed and the replay cursor starts over. This is a
new `response.create`, not a resume of the rejected generation. Both attempts
remain in request logs with resume capabilities redacted. Repeated expiry,
unrelated 400 errors and errors after response events are not blindly replayed.

Before a generation frame has been sent, five failed handshakes trigger SSE
fallback for that request. The next request tries WebSocket again. Authentication,
policy, validation, and rate-limit errors are returned directly; HTTP 404, 405,
and 426 remain eligible for transport fallback. A 403 during handshake or in a
stream enters the existing 30-minute Basispoints cooldown. A manual disable
continues to take precedence over automatic recovery.

The downstream history bridge remains enabled, including materialization of
`previous_response_id`. Account selection and rotation remain unchanged: lookup
reuses the socket for the already selected credential, caller, session and proxy.
It never pins account selection to a prior request. Native hosted-tool routing
and the native Codex transport remain separate.

Runtime logs record only WebSocket connection establishment and closure, using
the existing access-log `session_id` prefix (up to eight characters). No separate
random connection ID is generated for display. Native Codex connection logs use
the same session prefix. Connection logs have no `turn_id`; request logs retain
their turn identity. Access logs show `POST/WS` for HTTP clients using an
upstream WebSocket and `WS/WS` when both sides use WebSocket; HTTP fallback keeps
the ordinary HTTP method label. This formatting does not change request methods,
or the native Basispoints request and resume protocol fields.

## Image attachments

User-message data-URL images are decoded and uploaded to
`/basispoints/api/attachments` as a multipart `file` before generation. The returned
`openai_file_id` becomes the Responses `input_image.file_id`; image detail is kept.
The whole request is checked before the first upload: JPEG, PNG, GIF and WebP are
accepted only when the declared MIME type agrees with the file signature.
JPEG/PNG MIME aliases are normalized. Upload filenames use fixed `.jpg`, `.png`,
`.gif` and `.webp` suffixes rather than the host's MIME-extension database.

Tool-result data-URL images receive the same validation but remain inline. Their
bytes and detail are preserved, with only MIME aliases normalized. HTTPS image
URLs and existing file IDs remain unchanged because their bytes are not available
for local validation; opaque URLs are not rejected for lacking a suffix.

## Validation

Local WebSocket servers cover the routing matrix, native authentication and
request frames, streaming and non-streaming tool conversion, resume tokens and
cursors, deduplication, cancellation, handshake fallback, 403 cooldown, and full
error logs with request logging both enabled and disabled. Existing Basispoints
tests cover optional fields, model/effort passthrough, account rotation, encrypted
tool semantics, and turn identity. These are local protocol tests, not claims of
successful generation against an authenticated production Basispoints account.

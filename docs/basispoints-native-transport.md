# Basispoints native request and WebSocket transport

## Independent Office authorization

Basispoints now requires its own Office OAuth grant; Codex and SIWC tokens are
never substituted. The browser PKCE flow follows sub2api's Office profile and
official extension build `1793753700`. The fixed Office client is
`app_fnr0pYvVwwFDocDumLG3H2Bp`; token exchange and refresh use
`https://auth.openai.com/oauth/token?unified=true`, without the Codex refresh scope.
The registered callback remains on the official extension origin and is pasted
back into the management UI. No loopback listener or default device flow is added.

Bind an existing account with `POST /v8/management/oauth/basispoints/bind` and
`{"name":"codex.json"}`. Submit its callback through the shared v8 OAuth callback
endpoint with `provider:"basispoints"`, poll/cancel using the shared OAuth session
endpoints, and read status at `/v8/management/oauth/basispoints/status?name=...`.
The response exposes `off`, `valid`, or `invalid`, without credential values.

Binding atomically saves the independent `basispoints` partition and enables
`use_basispoints`. Saving that switch as `false` disables use while retaining the
grant. Both the global `codex.basispoints.enabled` switch and the account's
`use_basispoints` switch must be true to route through Basispoints. The global
switch is the master gate; disabling it retains account preferences and grants
but bypasses Basispoints for every account. An absent account switch is disabled.
Missing Office authorization returns `basispoints_authorization_required` before
upload or generation. Workspace, user identity, Office client, and ID-token audience
are checked. Office rotation is serialized by the credential file lock and saved
before use; failures and HTTP 401 rejection affect only the Office grant.

Office routing precedes the Codex runtime; native capability handoff still uses
the original native path and its credentials. Per-account selection enables
the full-history bridge only while the global gate is enabled. Payload rules run
once after built-in normalization,
attachment upload, and fast-mode adaptation, before transport framing.

Codex runtime selection uses only the ordinary OAuth partition while both runtime
switches are enabled and Office routing is inactive. The ordinary credential's
expiration remains enforced; an unused SIWC token cannot block the worker.
Disabling runtime restores selection by the SIWC switch. Disabled accounts,
terminal authorization failures, and quota blocks still apply.

## WebSocket control semantics

Cancellation sends `basispoints.response.cancel` with the current request ID and
resume token before closing, when the transport writer is available. It never
waits for an active large write to finish before closing that socket.

Server draining retires the connection and rotates affinity, including while idle.
An initial draining rejection can recreate a generation only within the configured
request-retry budget, before a response or `upstream_sent` marker and outside
recovery. Draining during recovery or after dispatch never regenerates the request.

The four `basispoints_resume_not_found/gap/invalid/already_attached` errors terminate
recovery with the original error. Resume-not-found rotates affinity only during a
pending resume and with `error.safe_to_rotate_affinity:true`. No connection-age
rotation or new network deadlines are introduced.

## Structured output and client history validation

`text.format` supports `json_object` and `json_schema` through a developer
instruction and local terminal-answer validation. This is not upstream constrained
decoding. JSON-object mode requires an object, not a scalar or array. Schemas
resolve in-document references only and cannot fetch network or file resources.
Ordinary text remains incremental; structured message events wait for the terminal
response, then use its validated message items. Intermediate message snapshots and
deltas never escape validation. Tool turns and refusals retain their own protocol
semantics. Incomplete structured answers fail instead of publishing partial JSON.
Payload rules remain the final request mutation stage and can override the injected
instructions; the bridge never restores them after configuration.

Historical `additional_tools` and client `tool_search_output` batches can replace
a callable contract. Conflicts inside one batch and ambiguous namespace aliases
remain errors. Earlier calls rebuild with the tool definitions active at their
history position, while new responses use the latest catalog. Cached replay also
checks a canonical fingerprint of the client call, including exact custom input
and encryption metadata, so a reused call ID cannot overwrite edited arguments
with an old native item. JSON object ordering and transport IDs do not affect a
match.

Plaintext standard `update_plan` calls require one unambiguous declared function.
Alias conflicts, unknown statuses and arguments outside the declared schema fail
before client tool events are emitted. Encrypted argument fields remain opaque.

Both `error` envelopes carrying `basispoints_server_draining` and the named
`basispoints.response.server_draining` control event retire the worker. The same
pre-dispatch retry guard applies to both; neither can restart an already dispatched
or recovering generation. Idle and just-completed connections are retired too.

The existing shared 403 cooldown policy is unchanged: it still affects protocol
selection for 30 minutes rather than credential availability. Per-account cooldown
and routing changes need a separate policy decision.

## Existing request and transport contract

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

Image generation and editing API requests use the existing native Codex image
executor before Basispoints or Responses WebSocket selection. The existing
request detector checks the openai-image source format and the generations/edits
endpoint path, not a model name or prefix. This applies to streaming and
non-streaming requests, including multipart edits. Completed image_generation_call
history alone still does not require a native route.

Explicit native tool selections and native hosted-tool continuations are routed
by CodexAutoExecutor before entering Basispoints. They do not upload Basispoints
attachments, send a Basispoints request, or dial its WebSocket. The complete client
request remains authoritative for the native channel's tool declarations and history.

Optional hosted declarations under automatic tool selection no longer force a
native route. Their capabilities remain described in the Basispoints prompt. If
the model actually needs one, it requests the exact cpa.native/TYPE marker through
run_officejs. The proxy forwards the original request once to native Codex, without
dispatching any speculative client tools. Malformed tool output, transport errors
and upstream rejections do not trigger this capability switch or a correction.

For streaming requests with optional native capabilities, the candidate Basispoints
response is buffered until the complete tool batch resolves the channel decision.
This delays downstream text and tool events even when the result stays on Basispoints.
If native Codex is needed, the first candidate response is withheld and only the
native response is published. Both upstream generations retain their usage records.
This is a capability switch, not a format-repair retry. Client-only streams retain
incremental delivery. Cancellation does not start a native request.

Native requests do not close an already retained Basispoints socket. The existing
native transport policy still determines HTTP versus WebSocket.

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

## Client function source transport

The bridge uses the existing `run_officejs` envelope for client tools. In addition
to CUSTOM raw input, eligible FUNCTION tools use an explicit source marker:

- `cpa.function_code/<exact-catalog-name>` for an object schema declaring a string
  `code` property.
- `cpa.function_cmd/<exact-catalog-name>` for `exec_command` functions declaring
  a string `cmd` property, unless the function qualifies for FUNCTION_CODE.

The original source is placed directly in `code`. `extended_summary` contains
one JSON object with every other supplied argument, or `{}`. The bridge combines
these fields and serializes the client arguments with Go's JSON encoder. It does
not evaluate, trim, infer a target from, or rewrite source text. Exact duplicate
source fields are accepted; conflicting duplicates remain protocol errors.

SSE, WebSocket and aggregated responses use the same converter. Cached native
items replay unchanged; complete uncached plaintext history uses the raw
transport when its current catalog supports it. Older JSON envelopes and
unavailable historical tools retain their existing compatibility path. Direct
encrypted calls retain their existing metadata and history handling. No model
correction, regeneration request or automatic tool retry is added.

Reference: `ranxi2001/sub2api` at
`2a0d3fc4d669aeaa7d5dc2b91dbf58fd044b852a`,
`backend/internal/service/basispoints/function_code_transport.go` and
`function_cmd_transport.go`. Only the raw source transport is adopted, not its
tool-omission policy or corrective model requests.

## Client-executed tool discovery

A tool_search declaration with execution=client is a client capability. Its search
arguments use the existing client transport and return as a tool_search_call with
execution=client, an object-valued arguments field, and the original call_id.
Streaming publishes output_item.added/done rather than function-argument deltas.

The matching tool_search_output loads its returned tool definitions into the next
request's catalog. Its complete result and call identity replay through the native
function transport. Complete history also rebuilds after cache loss or account
rotation. Descriptive annotation changes do not create a conflicting duplicate tool;
the callable argument contract must still agree. Server-executed tool search remains
a native capability.

Protocol reference: https://developers.openai.com/api/docs/guides/tools-tool-search

## Validation

Local WebSocket servers cover the routing matrix, native authentication and
request frames, streaming and non-streaming tool conversion, resume tokens and
cursors, deduplication, cancellation, handshake fallback, 403 cooldown, and full
error logs with request logging both enabled and disabled. Existing Basispoints
tests cover optional fields, model/effort passthrough, account rotation, encrypted
tool semantics, and turn identity. These are local protocol tests, not claims of
successful generation against an authenticated production Basispoints account.

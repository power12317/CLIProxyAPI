# ChatGPT Codex upstream WebSocket preference

`codex.force-websocket` defaults to `false`. It changes the upstream transport for
Codex generation targeting `https://chatgpt.com/backend-api/codex`, including the
implicit default destination. It does not rewrite custom provider destinations or
change downstream authentication, protocol, or response format. Standalone
`responses/compact` continues to use its HTTP endpoint.

```yaml
codex:
  force-websocket: true
```

The management API exposes `GET`, `PUT`, and `PATCH`
`/v0/management/codex/force-websocket`. Updates accept `{"value": true}` or
`{"value": false}`. The manager panel places the setting next to WebSocket
authentication; these settings are independent.

## Source audit

The implementation was checked against OpenAI Codex CLI `rust-v0.156.1`
(published September 23, 2026), with the September 24 main snapshot
`51d45620702cd3c825d3520ddc7d7e8052a16b87` reviewed for subsequent connection fixes:

- [Client lifecycle and request construction](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/client.rs)
- [Actual WS serialization schema](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/codex-api/src/common.rs)
- [WS connection and event processing](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/codex-api/src/endpoint/responses_websocket.rs)
- [Per-turn metadata and compatibility headers](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/responses_metadata.rs)
- [Retry and fallback decisions](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/responses_retry.rs)
- [Current main connection-closed detection](https://github.com/openai/codex/blob/51d45620702cd3c825d3520ddc7d7e8052a16b87/codex-rs/codex-api/src/endpoint/responses_websocket.rs)
- [Public WebSocket guide](https://developers.openai.com/api/docs/guides/websocket-mode)

The actual CLI schema is authoritative for the ChatGPT backend. In particular,
`ResponseCreateWsRequest` serializes `stream: true` and optional `stream_options`.
Removing them based on the public guide would lose native reasoning-summary
delivery controls. Background HTTP requests are not WS generation requests.

## Credential-owned physical connections

The September 26 correction makes each ChatGPT Codex credential own at most one
physical WebSocket **per CPA process**, shared across executor instances, models,
callers, logical sessions, and downstream JSON/SSE/WebSocket transports. Independent
CPA processes or replicas do not share a socket. The registry key is the stable
auth ID, never a model, session, token revision, or proxy URL.

Only a failed physical read/write (including a peer close or TCP failure) retires
the connection. Retirement closes the underlying socket and finishes its sole
reader before making the credential eligible for another dial. A later request
may reconnect; there is no background reconnect. Cancellation, a completed
response, overload, idle time, stream exhaustion, executor replacement, config
changes, session eviction, and Home selection release cannot close a healthy
physical connection. Process exit necessarily ends process-owned sockets.

Token refresh, model/tier changes, and proxy configuration changes do not rotate a
live connection. Its original handshake and transport remain in effect until it
ends. A request for a different account owner or endpoint under the same auth ID
fails explicitly instead of using another owner's authenticated socket or opening
a second connection. A revoked credential is still excluded by normal selection;
retaining its idle transport does not authorize new requests with it.

Parallelism uses the public Responses WebSocket `stream_id` protocol. The sole
reader routes events to logical subscribers; the write lock covers only a frame
write. It does not cover generation. Slots use a fixed set of at most 32 names,
recycling an idle slot rather than creating a new name or a new connection for
every conversation. Continuation optimization is invalidated when a slot changes
owners. Full slots return a request-scoped capacity error without dialing again.
The public service documents at most 16 active responses; CPA cannot remove an
upstream concurrency limit or promise unlimited simultaneous generations.

A cancelled subscriber stops receiving events while the physical reader drains
its response. Its slot cannot be reused before a terminal boundary. Private
steering that still has unacknowledged or accepted-but-unresolved input retains
its slot until the corresponding successor/failure establishes that boundary.
Each subscriber has a bounded buffer (256 events, 8 MiB); a slow reader fails its
own logical stream while other streams continue. It cannot close the shared
socket or stall the global read loop.

This multiplexing layer is an extension beyond Codex CLI **0.157.1**:

- [CLI request schema](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/codex-api/src/common.rs#L313)
  does not declare `stream_id`.
- [CLI stream lock](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/codex-api/src/endpoint/responses_websocket.rs#L275)
  is held for the entire response stream, rather than using a multi-stream dispatcher.
- [The public WebSocket guide](https://developers.openai.com/api/docs/guides/websocket-mode)
  explicitly documents concurrent named streams and echoed stream IDs on events.

**ChatGPT backend support still requires live verification.** The supplied HAR
has 11 sequential creates, no `stream_id`, and unlabelled Codex metadata events.
It does not establish that `chatgpt.com/backend-api/codex/responses` enables the
public multiplexing extension. The implementation does not guess metadata owners:
an event without a supported, unambiguous route fails affected requests and marks
the connection unusable for further requests while keeping the physical socket
open. It does not silently serialize, switch to SSE, replay, or open another
connection. Credential-wide rate-limit/timing events are not assigned to a turn.
Private steering acknowledgements may be routed by their known parent response.
Internal CPA stream names are removed before forwarding to downstream clients.
An existing downstream stream name is restored. The existing downstream WS
execution session represents one logical stream; changing its stream name during
that session fails explicitly. Independent downstream sessions execute in parallel.

Physical connection logs have `scope=credential` and a stable `connection` ID for
the lifetime of the socket. Only actual connects/disconnects are logged; logical
reuse does not emit an `upstream connected` line.

## Ordering and parameter ownership

| Stage / field | Behavior |
| --- | --- |
| Credential and route selection | Finish before dialing; respect the resolved proxy and credential owner. |
| Handshake | Use the existing OAuth/API-key identity pipeline, account header, configured overrides, cookie jar, routing hint, session/thread identity, and `OpenAI-Beta: responses_websockets=2026-02-06`. |
| `generate:false` | Forward a client's warmup on WS and consume its completion before the next request. No unsolicited model warmup or background reconnect is started. If all five handshakes fail, acknowledge a local warmup without issuing a billable HTTP generation. |
| Generation frame | Send `response.create`; preserve native tools, reasoning, metadata, stream options, and Responses Lite settings. |
| `response.id` | Use as `previous_response_id` only on the same live connection and only when the retained input/output prefix and request properties match. |
| Response output | Include completed tool/reasoning/message items in the comparison baseline. Incomplete or failed responses are not continuation baselines. |
| `x-codex-turn-state` | Prefer a valid `response.metadata` value; use `codex.response.metadata` only as a fallback. In forced mode, retain the first value from the preferred source for the same credential owner, origin, and turn. A standard event may replace an earlier fallback value. Replay it in `client_metadata` on subsequent frames and in HTTP headers on fallback. New turns and changed owners do not inherit it. |
| WS handshake turn-state | Native CLI passes no turn-state capture to its connect operation. Forced mode does not promote a handshake-only token into the turn cache. Dynamic state belongs in frames, not reconnect handshake headers. Explicit header/model overrides and the separately configured ticket feature retain their existing precedence. |
| Metadata on reused connections | Rebuild each frame's metadata from the current request. Reusing a socket does not mean reusing the previous turn's metadata. |
| Model / Fast / `service_tier` changes | Send current settings in each frame while retaining the credential socket and its original handshake. The next connection, after an actual transport closure, uses current handshake settings. |
| ETags, rate limits, model and usage events | Preserve existing event/usage handling. These response values are not blindly copied into request headers. |
| Connection closed | Invalidate the socket and connection-local response continuation. A later request dials again. No detached reconnect task runs. |

The existing fork-specific identity convergence, OAuth fidelity, reasoning replay,
model routing, tool schema handling, and turn-state ticket options remain in their
current pipeline. This feature does not try to implement attestation or invent
missing native client identifiers.

## HAR alignment and metadata compatibility

A local September 24 capture of OAuth Codex CLI 0.156.1 contains two connections,
11 `response.create` frames (two prewarms and nine generations), and 11
`codex.response.metadata` events carrying turn-state. The captured client sends
none of those values back, including during repeated requests in the same turn.
The capture is private test input and is not part of this repository's fixtures.

The official source nevertheless implements the full turn-state path:

1. [`ResponsesStreamEvent::turn_state()`](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/codex-api/src/sse/responses.rs#L219)
   accepts only `response.metadata` and reads `headers.x-codex-turn-state`.
2. [WebSocket event processing](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/codex-api/src/endpoint/responses_websocket.rs#L747)
   stores the extracted value in a `OnceLock` supplied by the
   [actual caller](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/client.rs#L2003).
3. [Subsequent frame construction](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/client.rs#L1908)
   inserts that cached value into `client_metadata["x-codex-turn-state"]`.
4. [The documented lifecycle](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/core/src/client.rs#L275)
   keeps the value stable within a turn and clears it for a new turn or owner.

The captured event name does not pass the official extractor's guard. CPA's
support for `codex.response.metadata` is an intentional compatibility extension,
not a claim that the unmodified CLI recognizes both names. `response.metadata`
has priority regardless of arrival order. An empty or invalid standard value
does not block the fallback. If a fallback value has already been used, a later
valid standard event replaces it for subsequent requests; already-sent frames
cannot be changed. Forced mode keeps the first standard value, or the first
fallback value if no standard value arrives. Explicit configured overrides and
the optional ticket feature still take precedence over this passive cache.
Forced mode resolves turn identity from each frame before falling back to a
handshake header, so an old downstream handshake cannot route a new turn through
the previous turn's cache. An explicitly empty prewarm turn stays unscoped.

Other capture-driven corrections:

- A `response.create` without a parent is a complete transcript root. The capture's
  first generation keeps its seven input items instead of retaining a stale eighth
  `additional_tools` item from prewarm. The requested exception preserves the
  prewarm merge if either input's structured tool inventory contains `image_gen`;
  a textual mention in a prompt does not trigger it. Subsequent parent-linked
  requests still retain full replay history and derive safe wire deltas.
- Native `generate:false` frames with `request_kind:prewarm` preserve empty outer
  and nested `turn_id` values. Generation requests retain the existing fallback
  identity behavior.
- Native OAuth WS preparation does not synthesize `Accept: text/event-stream`,
  the legacy `Conversation_id` alias, or a handshake Lite header from a body-only
  Lite marker. Explicit header overrides remain effective, and each frame keeps
  its Lite mirror. HTTP response negotiation is unchanged.
- Legacy/custom WS transports retain their heartbeat liveness handling. The
  credential-owned ChatGPT connection has no post-handshake idle/read deadline
  and answers Ping without adding a write deadline. Idle time alone never
  triggers local closure or replacement.

Compression negotiation remains library-specific: the captured CLI offers
`permessage-deflate; client_max_window_bits`, while Gorilla offers
`permessage-deflate; server_no_context_takeover; client_no_context_takeover`.
CPA advertises the extensions its transport actually supports; it does not spoof
unsupported compression parameters. Wire behavior is therefore not byte-for-byte
identical to the native CLI.

## Retry boundary

One logical request owns one failure counter, shared across auth and bootstrap
attempts. A first handshake failure counts as failure one; the fifth causes HTTP
fallback for this logical request only. Bounded backoff between failed handshakes
is cancellation-aware. Authentication and quota rejections remain credential
errors rather than consuming transport fallback attempts.

This deliberately differs from Codex CLI's default five **retries**, immediate
426 fallback, and session-persistent HTTP fallback. The next CPA request starts
with a fresh WS preference and budget.

Once a request frame has been sent, an ambiguous transport error is request-scoped
and stops outer bootstrap/credential replay. A lack of downstream text does not
prove that upstream tools or generation have not executed. Mid-response SSE/WS
splicing is not performed.

## Replay, isolation, and resource limits

For ordinary downstream Responses WS sessions, forced mode retains canonical
history by replacing complete roots and merging parent-linked deltas, with the
image-tool prewarm exception described above. The executor independently derives
a wire delta when safe. A reconnect or HTTP fallback can therefore use the full
request. Unknown previous-response IDs are rejected rather than guessed.

Both continuation retention and downstream replay history are bounded at 8 MiB.
Larger full requests still execute; clients must supply a full replay when retained
history is unavailable. Full-duplex steering remains tied to its live upstream;
HTTP cannot transparently resume an active steering exchange.

HTTP clients still use logical execution-session leases to isolate replay and
request state by caller, canonical session, route model, and request proxy. These
leases no longer own ChatGPT physical sockets. Missing caller scope disables
logical continuation reuse, but does not disable credential-level socket reuse.
Closing/draining a logical pool never drains the credential transport registry.
Custom upstream endpoints retain their existing handshake-fingerprint lifecycle.

Home selection callbacks own logical subscriptions. Replacing or draining a
selection releases that selection and its request resources without closing a
healthy credential socket. Physical disconnect releases retained selection
ownership. No Home wire messages or release tuple formats change. A separate
Home server deployment has not been tested.

## Validation

The September 26 credential-registry tests use local WebSocket servers. A barrier
requires all ten independent creates to arrive on one physical connection before
any response starts, then returns interleaved responses and metadata. Assertions
cover peak live sockets of one, JSON/SSE/native WS routing, metadata priority and
turn isolation, active executor replacement, cancellation/draining, overload,
Home selection release, lazy redial after peer close, non-replay after ambiguous
failure, slow subscribers, 32-slot capacity, and 96 successive logical sessions
without creating additional stream names or sockets. Separate credentials retain
independent connections. These tests do not certify the private ChatGPT endpoint.

Validation for the September 26 correction: `go test ./...`, the required server
build, and targeted `go test -race` for multiplexing, executor replacement, Home
selection, fallback, and downstream overload handling passed. No live account was
used, no management-panel change was needed, and no production image was published.


Tests cover the exact five-failure boundary and fresh next-request budget, SSE
translation, native stream options and identity headers, socket reuse, turn-state
first-value and turn isolation, full-prefix continuation, unknown-parent rejection,
post-send replay prevention, bounded exclusive leases, Home ownership for SSE,
and independent management/YAML settings. Test servers and injected HTTP transports
are local; no production ChatGPT credentials are needed or used.

Validation on September 24, 2026:

- `go test ./...`: passed.
- `go build -o test-output ./cmd/server`: passed; temporary binary removed.
- Targeted `go test -race` covering the new executor, continuation, pool,
  downstream bridge, and Home lifecycle tests: passed.
- CPA-Manager-Plus type checking and production build: passed.
- CPA-Manager-Plus web tests: 3,438 passed; repository tests: 213 passed.
- Panel lint: no errors; five warnings in existing, unmodified code.

The subsequent HAR-alignment branch also passed the full Go suite, the required
server build, and targeted race tests for event priority, per-frame turn identity,
native OAuth headers, root replay, and heartbeat liveness. An offline replay of
all 11 captured requests matched the original wire input arrays and parent IDs.
Separate sanitized tests cover the image-tool merge exception and both metadata
event names, including fallback-first arrival. The manager UI did not require
further changes for these protocol corrections.

The setting remains disabled by default. This validation does not include a live
ChatGPT account or a separately deployed Home server.

## Branch image publication

The `codex/websocket-har-alignment` branch uses Git tags named
`websocket-har-alignment-vYYYY.MM.DD-N`. These tags trigger only the dedicated
`docker-websocket-har-alignment` workflow. The regular release workflow excludes
them, and the production Docker workflow's `v*` filter does not match them.

Branch images publish to the separate GHCR package
`ghcr.io/power12317/cliproxyapi-websocket-har-alignment`, tagged with the full Git
tag. The workflow builds Linux amd64 and arm64 images and combines them into a
multi-architecture manifest. It writes no `latest`, `latest-amd64`, or
`latest-arm64` tags and never targets the production `cliproxyapi` package.

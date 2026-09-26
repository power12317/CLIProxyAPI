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
| Fast / `service_tier` changes | Reuse an eligible live OAuth socket when only the tier changes. Send the new tier in the request frame; the existing handshake remains unchanged. The next fresh connection uses the current model/tier routing hint. |
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
- Ping and Pong renew the existing WS read deadline for both pooled and raw
  connections. The capture's 371-second heartbeat-only gap between prewarm and
  generation should not close a healthy socket at five minutes. Reconnection
  remains lazy and begins only when a later request needs a connection.

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

The September 26 correction restores the session-level transport from commit
`69191389a4be711dda088c55e668ce2278cd7233` and gives explicitly identified topics a
stable owner independent of the downstream socket's temporary execution ID.

### Topic identity and child threads

The private ChatGPT transport follows Codex CLI `rust-v0.157.1`:

- [`ModelClientState`](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/core/src/client.rs#L198)
  owns a session-local connection cache. New turns borrow that cache and return it
  when their `ModelClientSession` is dropped.
- [Child spawning](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/core/src/agent/control/spawn.rs#L703)
  creates a separate thread. [Session construction](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/core/src/session/session.rs#L1700)
  creates its own `ModelClient`, so children have independent connection caches.
- The native response stream holds exclusive access to its socket until the
  response ends. Independent topics can run concurrently without sharing their
  response event stream. Existing native steering remains inside its topic.

Resolution prefers a thread ID, then a session ID, then `prompt_cache_key`.
Per-frame `client_metadata`, its nested `x-codex-turn-metadata`, and explicit body
fields take precedence over stale handshake fields of the same kind. Handshake
`Thread-Id`/`Thread_id`, `Session-Id`/`Session_id`, and `X-Codex-Turn-Metadata`
remain supported. Turn IDs never define connection ownership. Parent IDs never
stand in for a child's missing identity, and an explicit child thread wins over
its shared session or cache key. These values are not universally interchangeable:
different explicit thread IDs remain separate even when cache affinity matches.

The internal topic key is scoped by downstream caller, credential owner, and
upstream endpoint. It is never inserted into the upstream protocol. It excludes
the model, turn, and temporary downstream connection ID. Client-supplied wire
identifiers retain the existing fidelity/identity pipeline; generated cache
identity uses the stable topic instead of the temporary downstream connection.
A request without a resolvable topic retains the previous execution-session
behavior rather than being grouped with unrelated requests.

### Connection lifetime

A topic holds one upstream socket. Later rounds and reconnected downstream
WebSockets reuse it. An overlapping owner waits with cancellation support; it
does not create a second socket or receive a local busy error. Native duplex
steering retains ownership until its downstream exchange finishes. Distinct
parent and child topics execute independently.

A cancelled consumer detaches from the reader. The reader drains unfinished
native responses before another consumer takes the topic, including accepted
steering successors. A successor waiting for required input can be continued by
the next owner. Metadata without a response ID is delivered as part of the
current native response; it is not treated as a transport failure.

Request-level overloads and response failures do not close a healthy topic
socket. Configuration reload and downstream-session cleanup do not close it
either. Model and per-turn changes use current request frames, while the existing
handshake remains fixed. Existing model routing and steering replay checks still
apply. Current connection settings are used when an actual disconnect requires a
new connection. There is no background reconnect. Existing liveness deadlines
remain in effect; this change adds no post-handshake network deadline.

Physical connection logs include `scope=topic`, a stable hashed `topic`, and a
fresh `connection` ID for each physical socket. A connected log means a new
handshake succeeded. Reuse emits no connected log. Disconnect logs preserve the
physical ID and reason. Shutdown and explicit credential removal still release
resources.

Home selection retention follows the upstream topic lifetime. Closing a
downstream execution session does not end its retained topic selection. A later
selection can replace logical accounting without closing the transport; actual
disconnect and shutdown release retained ownership. No Home wire message or
release tuple changes. The Home server repository was not available locally;
CPA tests cover its existing selection/resource contract.

Requests without topic identity retain the bounded legacy pool (128 idle
leases), including its existing cleanup and isolation rules.

## Validation

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

The WebSocket repair branch `codex/websocket-event-routing-compat` uses the
existing isolated publication scheme from `codex/websocket-har-alignment`:
`websocket-har-alignment-vYYYY.MM.DD-N`. These tags trigger only the dedicated
`docker-websocket-har-alignment` workflow. The regular release workflow excludes
them, and the production Docker workflow's `v*` filter does not match them.

Branch images publish to the separate GHCR package
`ghcr.io/power12317/cliproxyapi-websocket-har-alignment`, tagged with the full Git
tag. The workflow builds Linux amd64 and arm64 images and combines them into a
multi-architecture manifest. It writes no `latest`, `latest-amd64`, or
`latest-arm64` tags and never targets the production `cliproxyapi` package.

## Topic ownership regression coverage

Local WebSocket tests exercise the real downstream handler and Codex executor in
both ordinary and steering modes. They cover repeated turns, downstream
reconnection, header/body identity aliases, child isolation with shared cache
keys, per-topic cancellation/draining, overload reuse, reload retention, lazy
reconnection after a physical close, and turn-state priority. The peers emit
native metadata/created/completed events without any additional routing field.
Deterministic synchronization tests cover cancellation while waiting and native
steering successors. No live ChatGPT credentials are used by these tests.

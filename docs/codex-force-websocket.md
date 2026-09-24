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
| `x-codex-turn-state` | In forced mode, use the first returned metadata-event value for the same credential owner, origin, and turn. Replay it in `client_metadata` on subsequent frames and in HTTP headers on fallback. New turns and changed owners do not inherit it. |
| WS handshake turn-state | Native CLI passes no turn-state capture to its connect operation. Forced mode does not promote a handshake-only token into the turn cache. Dynamic state belongs in frames, not reconnect handshake headers. Explicit header/model overrides and the separately configured ticket feature retain their existing precedence. |
| Metadata on reused connections | Rebuild each frame's metadata from the current request. Reusing a socket does not mean reusing the previous turn's metadata. |
| ETags, rate limits, model and usage events | Preserve existing event/usage handling. These response values are not blindly copied into request headers. |
| Connection closed | Invalidate the socket and connection-local response continuation. A later request dials again. No detached reconnect task runs. |

The existing fork-specific identity convergence, OAuth fidelity, reasoning replay,
model routing, tool schema handling, and turn-state ticket options remain in their
current pipeline. This feature does not try to implement attestation or invent
missing native client identifiers.

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
history using the existing request merger. The executor independently derives a
wire delta when safe. A reconnect or HTTP fallback can therefore use the full
request. Unknown previous-response IDs are rejected rather than guessed.

Both continuation retention and downstream replay history are bounded at 8 MiB.
Larger full requests still execute; clients must supply a full replay when retained
history is unavailable. Full-duplex steering remains tied to its live upstream;
HTTP cannot transparently resume an active steering exchange.

HTTP clients use exclusive execution-session leases, isolated by authenticated
caller scope, canonical session, route model, and request proxy. The socket also
checks credential owner, endpoint, resolved proxy, and handshake fingerprint.
Requests without an authenticated caller scope do not reuse connections across
requests. At most 128 idle leases are retained per pool; active requests continue
to use the existing credential concurrency controls. There is no new idle timer
or generation timeout; existing WS liveness deadlines remain in effect.
Disabling the policy drains idle leases immediately and retires active leases
after their requests finish. Shutdown also drains the pool.

Home retains the selection and its bound resources for the upstream WS lifetime,
even when the downstream is SSE. Stream completion releases the request attempt,
not a retained socket's Home ownership. Disconnect, eviction, drain, and explicit
session closure release ownership. No Home wire message or release tuple format
is changed. The Home server repository was not available in this workspace;
integration tests exercise CPA's existing selection/resource contract.

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

The setting remains disabled by default. This validation does not include a live
ChatGPT account or a separately deployed Home server.

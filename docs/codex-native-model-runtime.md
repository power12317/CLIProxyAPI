# Native model runtime experiment

This document describes the `codex/native-model-runtime` branch. It does not
change the deployment contract of `main`.

## Scope and reference version

- CPA base: `e50b3414c86440f4bd1432685f19c48c4af6b76f`.
- codex-server fork base: `138acc222ee9510204f17c0d34bdf78995a4ca1b`.
- Official comparison baseline: Codex `0.162.1`, tag `rust-v0.162.1`, commit
  `092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`.

CPA retains its protocol translation, model mapping, business preprocessing,
and payload rules. The result is input to the native model executor, rather
than a specification of the exact upstream wire payload. Native input mapping,
normalization, transport, and model completion belong to codex-server.
The CPA entry sets worker retry budgets to zero and returns errors immediately;
CPA retains retry ownership under its existing policy.
Unsupported native input must fail explicitly. Neither CPA direct egress nor
the former bridge HTTP sender is a fallback for an enabled Runtime request.

Client tools are model input and output only. Function/custom definitions,
namespaces, calls, and subsequent caller-provided results do not register a
server tool executor. A response containing only tool calls can complete a
model invocation. It does not start an Agent tool loop.

## IPC boundary

The WebSocket RPC endpoint remains unchanged. Protocol version 5 separates
acceptance, live native model events, native termination, and diagnostics:

- `cpa/inference/start` returns when the first native business event is ready;
  it does not wait for model completion or diagnostic delivery.
- `cpa/inference/event {requestId, bodyBase64}` carries incremental
  business output. Text and client tool argument deltas are delivered while the
  model is still generating.
- `cpa/inference/completed {requestId}` confirms native success separately from
  the body. Failure after acceptance uses `cpa/inference/error`; failures before
  acceptance remain start RPC errors.
- `cpa/inference/upstream` records actual native exchanges, and
  `cpa/inference/diagnostics/completed {requestId}` ends that diagnostic stream.

CPA and codex-server must be upgraded together. Protocol 4 peers are rejected
explicitly; there is no buffered-output compatibility mode or CPA model fallback.
Native model execution is the only implementation. `executionMode:
"inference-only"` and `rawBody: true` describe execution ownership and framing,
not a selectable upstream transport.

Native diagnostics carry their actual `http` or `websocket` transport. With
`upstreamBodyLogs: true`, original attempt bytes are logged without logging the
business copy twice. Diagnostic HTTP errors never replace the native final
outcome. CPA receives the socket independently of diagnostic callbacks, and its
HTTP consumer flushes business output before joining trailing logs. The same
request ID associates CPA, master, worker, and native upstream exchanges.

The final model completion frame is released after the independent native
success confirmation. This terminal-only hold prevents a client that disconnects
immediately on `response.completed` from racing native success bookkeeping;
preceding deltas do not wait. A connection loss before native confirmation is a
request-scoped error and is never replayed by CPA. Delivery cancellation after
confirmed native completion is recorded as a delivery interruption, not a native
model failure.

CPA retains basic response translation and usage extraction. Images retain
their own result adapter rather than using Responses completion rules. Each
worker invocation makes a native model call with its retry budgets set to zero:
no worker recovery loop, retry after a 401, or WebSocket-to-HTTP replay. Normal
Codex CLI entrypoints keep their official recovery behavior.

Explicit native failures retain their status, body, headers, and typed error in
the bridge protocol. They enter CPA's existing error, credential-selection, and
retry handling. After Runtime is selected, subsequent CPA attempts remain on
Runtime-owned credentials; a mixed pool cannot fall back to CPA model egress.
Ambiguous IPC failure retains the existing no-replay classification. Once CPA
has delivered business output, a stream failure terminates that stream and is
returned to the client; it never concatenates another model attempt. The obsolete
worker attempt notifications and CPA per-attempt body reconstruction are removed.

Diagnostic consumers preserve CPA attempt order independently of business
receipt. A later attempt can deliver model output while the previous attempt's
logs are still draining. Failed-attempt logs remain attached to the correct
request/response exchange.

## Downstream WebSocket delivery

The earlier HTTP/WS transport matrix used an HTTP SSE client and varied the
worker-to-upstream transport. The additional WebSocket-client tests connect to
CPA's actual `GET /v1/responses` endpoint. Four combinations cover HTTP/WS native
upstreams with CPA forced-WebSocket mode off/on. Each upstream event is released
only after the client receives the preceding event, including tool deltas and
tool-done events. The same client connection then completes a second request.

This exposed two CPA completion-lifecycle issues: HTTP flushing of an already
hijacked WebSocket connection could panic, and per-inference diagnostic joining
could prevent the next request from starting. WebSocket writes now use only their
own transport. Diagnostic tasks finish in the background under the socket
handler's lifetime, in request order; they are joined before the handler's Gin
context is released. Late transport observations do not update a finished turn.

A separate blocked-diagnostic regression proves that the client can receive two
completed responses before the first diagnostic stream is released, and that
both upstream exchanges are retained in the final log. The complete real-worker
matrix passes 96 leaf cases with `-race`, alongside that diagnostic regression
and the relevant handler/helper package tests. This is a CPA-only correction;
IPC v5 and codex-server commit `6d9c1b9d5d3fe872e94608b428ae214e7ab90805`
remain compatible.

## WebSocket gateway node diagnostics

The native WebSocket records the `__oailb` gateway node from its actual handshake:
a response cookie takes precedence over the cookie actually sent, and no response
cookie falls back to that request cookie. An explicitly deleted or invalid response
cookie clears the node. The connection retains only the parsed node label, such as
`unified-107`, and includes it in every `response.create` diagnostic through the
existing `oaiLbNode` field. Closing the socket discards its diagnostic node.

CPA consumes this field through its existing request and access logging. No CPA
production change or IPC version change is required. The real-worker regression
`connection_node_reaches_each_request_log` checks both unary and streaming calls
on each reused socket, different nodes on separate sockets, and a fresh socket
without a cookie. It also asserts one handshake per pair and zero CPA model egress.
The old worker fails on the first call: its handshake reports a node but its next
`response.create` diagnostic clears that node in CPA.

The corrected worker passed 97 real-worker cases plus the delayed-diagnostic
WebSocket regression under `-race`, including live HTTP and WebSocket delivery.
The CPA node-consumer tests, access-logger tests and server build also passed.

## Native input compatibility

The official model entry accepts a native `Prompt`, typed tools and history,
reasoning options, structured output, verbosity configuration, and a service tier. It does not accept arbitrary final wire
extensions or a forced `tool_choice`. These cannot be recreated by patching
the upstream request after native construction.

The official standalone Images DTO accepts `prompt`, `model`, `background`,
`n`, `quality`, and `size`; edits also accept `images`. It does not expose
`mask`, `partial_images`, or `output_format`. Client streaming can adapt the
native final image result into image completion events, but cannot provide
upstream partial image events that the native Images operation does not expose.
Multipart images are first adapted by CPA into the same business input as JSON.
Unsupported fields must be reported rather than silently removed.

Hosted image declarations in ordinary Responses map to the official native
`ResponseItem::AdditionalTools` input. The fork-only `ToolSpec::ImageGeneration`
variant is not used by this entry. The declaration, partial-image events and
results passed both the independent official comparison and real-worker tests
against a mock upstream. This is not a live production service acceptance test.


The codex-server [input contract](https://github.com/power12317/codex-server/blob/codex/native-model-runtime/codex-rs/app-server/docs/cpa-native-model-runtime.md)
provides the complete supported-field table and the native execution boundary.

## Test images and deployment

Both repositories use branch `codex/native-model-runtime` and separate packages:

| Component | Experimental image |
| --- | --- |
| CPA | `ghcr.io/power12317/cliproxyapi-native-runtime:latest` |
| codex-server | `ghcr.io/power12317/codex-server-native-runtime:latest` |

Branch workflows build `linux/amd64` and `linux/arm64` and publish a combined
manifest. Immutable tags use `sha-<full-commit>`. The experimental workflows
must never publish into the main `cliproxyapi` or `codex-server` packages.
No release tag is needed to trigger the branch workflows.

Use both matching experimental images. The branch's `docker-compose.codex.yml`
defaults to them. After their image workflows have published successfully:

```sh
docker compose -f docker-compose.codex.yml pull
docker compose -f docker-compose.codex.yml up -d --no-build
```

For an existing server-specific Compose file, replace only the two image
references with the experimental package names. Retain its current network,
volumes, service names and port settings. codex-server continues to use CPA's
network namespace, the shared auth directory, its own persistent state volume,
and the existing loopback RPC listener. CPAMP remains an independent deployment
and communicates with CPA.

## Verification

The official comparison baseline remains Codex `0.162.1`, commit
`092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`. The earlier native-entry comparison
covered 16 scenarios using independent builds. This change intentionally differs
in retry ownership, as requested: errors return to CPA instead of native recovery.
It does not claim identical retry behavior to the ordinary official CLI.

The streaming test first failed against CPA `c3cfdb3c` and worker `4d86a145`:
the upstream sent `response.created` and paused, but the actual HTTP client
received no headers before the 20-second test watchdog. The corrected contract
is validated with phase barriers: a real HTTP client must receive the first
event, each of three tool-argument deltas, tool-input done, and output-item done
before the upstream is allowed to send completion. No server-side tool sentinel
may be created.

Error tests count actual upstream requests with provider retry budgets configured
above zero to verify that the CPA native entry overrides them. They cover native
HTTP and WebSocket errors before and after output, Images failures, cancellation,
and rejected WebSocket handshakes. Handshake failures must not cause a second
handshake or HTTP fallback. A separate CPA retry test uses a real HTTP client and
a mock master: one Runtime credential fails, CPA selects another Runtime
credential, and business output arrives while the first attempt's diagnostics
are deliberately blocked. A local credential between the two remains unused.

The real-worker matrices also cover Chat Completions, Images, Responses,
streaming/nonstreaming formats, JSON/multipart image edits, native normalization,
supported payload rules, Lite, compaction history, hosted images and partial
events, caller tools/results, actual cancellation, unsupported inputs, and mixed
credential pools. CPA's model transport is blocked and counted; model egress
must remain zero. Prefixed WebSocket metadata verifies actual outgoing ticket
bytes and correlated logs: `0/780` followed by `780/780` on the same connection.

Blocked-diagnostic tests validate start, incremental body, and native completion
independently of diagnostic callbacks. Actual HTTP JSON/SSE bytes are flushed
before diagnostic finalization. Native success remains confirmed if downstream
delivery is canceled afterwards.

The final paired macOS arm64 worker binary reports `codex-app-server 0.162.1`
and has SHA-256
`5c30250a6bfb252cacfba04507a5dcdb7496ee98d811341831777a4dfaa8566d`.
It includes the master correlation fix for repeated CPA request IDs while prior
diagnostics are still open. CPA retains the original request ID externally; the
master records its association with the existing internal worker RPC sequence.

The zero-retry streaming candidate was verified with 92 of 92 real-worker leaf
cases passing under the Go race detector. HTTP and WebSocket first-event times
were approximately 7 ms and 85 ms in the local fixture; these are delivery
measurements, not production model-latency guarantees. The independent delayed-
diagnostics CPA retry tests passed for both account rotation and the same worker.
The mixed-account test configured two CPA retries and observed three worker
requests and zero CPA model requests.

| Check | Result |
| --- | --- |
| Executor, helpers, bridge, API handlers, OpenAI handlers, auth manager, logging: complete package tests with `-race` | All seven packages passed |
| Updated CPA retry, route ownership and diagnostic lifecycle regressions with `-race` | Passed |
| Real worker HTTP/WS/error/handshake matrix with `-race` | 92 leaf cases passed |
| CPA retry while previous diagnostics are blocked, same account and account rotation | Passed |
| Required server build, formatting and whitespace checks | Passed |
| Middleware tests without `-race` | Passed |
| Wider middleware race check | Existing failure, independently reproduced on the unmodified base |

The wider race check detected
`TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger`: the existing
`ResponseWriterWrapper.Finalize` and `processStreamingChunks` race on the stream
channel field. It was reproduced from an untouched Git archive of `c3cfdb3c`,
using the same test and race detector. This change does not modify that middleware;
that race suite is not reported as passing.

The wider management test selection has one pre-existing failure:
`TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile/completed`. Its
OAuth fixture supplies an access token without the required email. The same
failure was reproduced on the unmodified CPA base `e50b3414`; this experiment does
not change that handler or fixture. It is not reported as a passing suite.

Reproduce the CPA checks from this branch, pointing at the matching worker binary:

```sh
go test ./internal/runtime/executor ./internal/runtime/executor/helps ./internal/runtime/executor/helps/codexruntime
CODEX_RUNTIME_TEST_BINARY=/path/to/codex-app-server go test -race ./internal/runtime/executor -run '^TestCodexRuntime.*RealWorker$' -count=1 -v
go build -o /tmp/cli-proxy-api-native-check ./cmd/server
actionlint .github/workflows/docker-native-runtime.yml
```

The tests use synthetic credentials and deterministic local services. Live
ChatGPT model acceptance and long-running production load were not tested.
Linux amd64/arm64 images are built by the two branch workflows; a triggered run
is not evidence of a published image. Use the immutable image tags for a recorded
pair once those workflows have completed successfully.

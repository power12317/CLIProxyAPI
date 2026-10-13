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
normalization, transport, recovery, and model completion belong to codex-server.
Unsupported native input must fail explicitly. Neither CPA direct egress nor
the former bridge HTTP sender is a fallback for an enabled Runtime request.

Client tools are model input and output only. Function/custom definitions,
namespaces, calls, and subsequent caller-provided results do not register a
server tool executor. A response containing only tool calls can complete a
model invocation. It does not start an Agent tool loop.

## IPC boundary

The WebSocket RPC endpoint remains unchanged; protocol version 4 describes the
final business bytes carried by `cpa/inference/completed.bodyBase64`. Native
model execution is the only implementation, with no execution selector or
alternate bridge sender. The existing `executionMode: "inference-only"` and
`rawBody: true` describe the model-only boundary and byte framing; they do not
select a model transport. CPA and codex-server use this same message contract.

`cpa/inference/upstream` is diagnostic data from actual native exchanges.
Its `transport` field identifies `http` or `websocket`. With
`upstreamBodyLogs: true`, `kind: "body"` carries the actual attempt bytes as
`bodyBase64`; final business output is not logged a second time. Attempt errors and HTTP
status codes are logged, but cannot replace the native executor's final state.
The same request identifier associates CPA logs and the master/worker call.

Native model failure and cancellation use the start RPC error, with the final
HTTP status, error body and safe response headers. If the worker exits after
the start reply but before completion, the master reports that interruption
through `cpa/inference/error`. CPA does not derive final failure from the last
failed attempt's diagnostics.

`cpa/inference/completed` confirms native success and carries the final
`bodyBase64` bytes in the same notification. This makes the final output and
success indivisible at the IPC boundary: a later connection close cannot turn
that confirmed result into a failure. A close before confirmed completion is
an uncertain bridge failure and is not replayed by CPA. A completion without
the model result needed for client protocol translation is an IPC contract
error, not an upstream model timeout.

CPA retains basic response translation and usage extraction. It does not run
its independent SSE failure/cooling classifier on native output. Ordinary
Responses and image output keep their respective result adapters; an image
event stream is not treated as a sequence of ordinary Responses completions.

The experimental response adapter buffers each native attempt and releases
business output only after native success. Failed attempts remain diagnostic
data. This avoids replaying text or tool calls into a client protocol that has
no attempt-reset operation. SSE framing remains available, but time to first
business output is delayed until the native model response completes. It is
not token-by-token live delivery. The native request and recovery policy does
not change with the downstream streaming choice.

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

During recovery, newly generated client tool calls from a failed attempt remain
uncommitted: they have not been delivered to the client and are not inserted as
unpaired calls into recovery history. The caller's original call/result history
is retained; the server never fabricates an aborted tool result. This explicitly
excludes the full Agent's local tool execution and history behavior from the
model-only comparison.

The codex-server [input contract](https://github.com/power12317/codex-server/blob/834df84ff6186aae690751f3576b392fa0e92cb5/codex-rs/app-server/docs/cpa-native-model-runtime.md)
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

The matched worker commit is `834df84ff6186aae690751f3576b392fa0e92cb5`.
Its final local macOS arm64 binary has SHA256
`902df54637d8820621d78cb782b6d7bdc0ad9e08c6b9fe03c9e8eec292e9287c`.
The independent official comparison used separate build directories and passed
16 of 16 scenarios against commit
`092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`. The worker's
[validation report](https://github.com/power12317/codex-server/blob/834df84ff6186aae690751f3576b392fa0e92cb5/codex-rs/app-server/docs/cpa-native-model-validation.md)
records the reference preparation, exact comparison rules, original-source
checks, model-only tool boundary and native component results.

CPA verification on the same host:

| Check | Result |
| --- | --- |
| Complete executor, executor helper and runtime IPC package tests | All three packages passed |
| Runtime/IPC/logging regressions with the Go race detector | Passed |
| Actual CPA handlers/executors to master and worker, HTTP/WS/native fallback | 114 leaf cases passed with `-race` against the final binary |
| Server build | Passed |
| Branch image workflow syntax | `actionlint` passed |
| Compose image resolution | Both experimental package names verified |
| Existing management toggle/reload tests | Targeted tests passed |

The real-worker matrices cover public Chat Completions and Images endpoints,
Responses, streaming/nonstreaming client formats, JSON/multipart image edits,
native input normalization, supported payload rules including verbosity, Lite,
compaction history, hosted images and partial events, client function/custom
calls and subsequent results, native partial-output recovery, actual cancellation
of HTTP/WS upstream work, explicit unsupported fields, and a mixed credential
pool that cannot fall back to CPA egress. All model requests are observed at the
local upstream, and CPA's model transport is blocked and counted: its count must
remain zero. Tool-name collision fixtures contain commands for a sentinel file;
the file must remain absent.

Diagnostic regressions check actual HTTP/WS transport reporting, readable
logging of native zstd requests without changing transmitted bytes, routing
metadata from WS frames or chunked/multiline SSE before completion, and no
repeated business-body log. Failed-attempt diagnostics cannot overwrite the
native final error. IPC loss cannot trigger outer CPA replay; final native bytes
and completion are tested as one notification.

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

# Compatible model input on both runtime branches

The October 22 compatibility changes apply to both `main` and
`codex/native-model-runtime`. Each branch keeps its existing model execution
architecture and image package. This change does not merge the experimental
runtime into the main runtime or introduce another user-facing mode.

## Execution and ownership

| Layer | Responsibility |
| --- | --- |
| CPA | Public protocol conversion, model mapping, configured business processing, and final user payload rules |
| Runtime input adapter | Equivalent input shapes, inferable defaults, complete tool declarations, and call/result relationships |
| Worker | Native request reconstruction, credentials, network connection, and the actual upstream model request |
| CPA response path | Public response conversion, streaming delivery, logging, and client-facing errors |
| Calling client | Execution of declared function/custom tools |

| Branch | Local protocol | Model execution | Image package |
| --- | --- | --- | --- |
| `main` | Raw HTTP body chunks; HTTP EOF completion | Existing native request builder and worker HTTP sender | `ghcr.io/power12317/cliproxyapi` with `ghcr.io/power12317/codex-server` |
| `codex/native-model-runtime` | Native model events; model terminal completion | Shared native model execution entry point, including its transport/session machinery | `ghcr.io/power12317/cliproxyapi-native-runtime` with `ghcr.io/power12317/codex-server-native-runtime` |

The main branch does not gain native upstream WebSocket continuation merely by
receiving these compatibility fixes. The native branch retains that capability.
Neither runtime path may fall back to CPA model egress when its worker fails.
Normal CPA execution with Runtime disabled retains its existing wire preparation.

## Input semantics

Runtime input is business data rather than a fixed ordinary-Agent template.
OpenAI Responses, Chat Completions, Claude, Gemini, and Interactions retain their
source protocol's semantics while mapping into the worker's input:

- Arbitrary function/custom names, namespaces, descriptions, input schemas and
  supported output schemas remain declarations. Names such as `WebSearch`,
  `ToolSearch`, `Bash`, and `Agent` do not select server-side executors.
- Complete raw JSON schemas retain constraints and extensions, including
  `$schema`, `default`, `pattern`, numeric bounds and large integer values.
  Gemini schema type aliases are normalized only at schema nodes; business
  values in `default`, `enum`, and `const` retain their case and values.
- Explicit `tool_choice`, `parallel_tool_calls:false`, `include:[]`, storage,
  sampling/token controls and structured-output contracts remain available to
  the worker. Missing values do not inherit unrelated Agent requirements.
- Chat nested function declarations and supported `input_schema` aliases map
  to the same declarations. Chat functions keep their non-strict source default.
- Inferable message shape and unique tool-result association may be completed.
  Duplicate, orphan or ambiguous associations and missing necessary tool names
  remain precise input errors. Tool arguments and results are never invented.
- Input adaptation does not use JSON round-trip equality as an admission gate.
  Known output-only metadata can be consumed without discarding actual text,
  reasoning, tool calls or compaction items.
- Authentication, transport timing, device identity and Lite transport metadata
  belong to the worker. Client copies do not replace worker-owned values.

CPA selects the business-input converter using a private request context. The
normal SDK translation envelope and its hooks still run. This is not a new
configuration flag, RPC parameter or capability-alignment setting.

CPA completes its built-in business transformations before applying configured
payload rules once. Its Runtime path no longer replaces caller tools by name,
strips valid schema constraints, or bulk-deletes model controls in anticipation
of an old HTTP template. The declared hosted-image stripping policy still
applies by tool type. The worker then normalizes and reconstructs its native
request; it is not a transparent raw-body proxy.

## Sessions, notifications and responses

Nested `client_metadata.session_id`, `thread_id`, and serialized turn metadata
participate in the existing scoped session association. Explicit headers retain
their precedence. Changing instructions or tools therefore need not create an
unrelated logical session.

Optional RPC notifications and malformed optional diagnostics do not manufacture
model errors or contaminate another request's log. Required RPC responses and
body/error/completion request identities remain checked. The old artificial
128-notification failure is removed; no fixed message/frame capacity is added.
Safe model response headers also retain `X-Codex-Safety-Buffering-*` metadata.

Responses continue incrementally through CPA. Tool-only completion ends the
model call successfully and returns the call to its client. The worker must not
execute client tools or start its own additional model attempt after an upstream
error; CPA owns any subsequent retry decision.

Independent image generation/editing, JSON and multipart masks, and streaming
or non-streaming image output retain their worker-owned paths. Images are not
classified as ordinary Responses solely to decide whether their stream ended.
Ordinary Responses compaction inputs and results remain separate from the
unsupported legacy standalone `/responses/compact` endpoint.

## Pairing and existing capability boundaries

Deploy the CPA and codex-server images from the same branch and compatible release
together. The shared RPC interface describes business notifications, completion,
diagnostics and transport capabilities directly. Product versions and source
revisions do not act as private protocol gates. Required missing behavior produces
a concrete capability error, and CPA never falls back to direct model egress.

The main HTTP runtime still cannot supply upstream WebSocket steering,
`generate:false` prewarm, asynchronous background retrieval, or external stored
history that has not been materialized. CPA's existing downstream continuation
handling can provide reconstructed history where it already supports that
operation. A remaining unsupported input must return its concrete error without
CPA direct egress. This change does not implement legacy standalone compact,
token counting, or per-request worker proxy reconfiguration.

## Verification

Unit and mock-master regression coverage checks all five input conversions,
complete schemas, source defaults, explicit false/empty values, association
errors, final payload rules, stable nested session identity, and optional
notifications. Existing normal CPA executor tests run alongside Runtime tests.

The actual-process tests run a real main-branch master and child worker with fake
credentials against a local upstream. CPA model HTTP is independently blocked
and counted; the upstream records requests actually sent by the worker.

```sh
CODEX_RUNTIME_TEST_BINARY=/absolute/path/to/main/codex-app-server \
  go test -race ./internal/runtime/executor \
    -run 'TestCodexRuntime(CompatibleRealWorker|ImagesRealWorker)$' -count=1 -v
```

The compatibility test checks five source protocols in streaming and
non-streaming modes, exact important schema values, structured formats,
namespaces and unique result association, explicit storage controls, final
payload semantics, client-owned tool completion, upstream failures and
cancellation. Its real HTTP client must receive each event before the fixture
releases the next generation phase. It records the first-event time and
intermediate delivery, not merely the final body.

The image test covers actual sender ownership for JSON/multipart generation and
editing, masks, both response modes, payload rules, normal compaction/image-tool
requests, cancellation and failure/no-direct-fallback scenarios. These are local
controlled tests; they do not claim live model or production-credential results.

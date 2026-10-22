# Compatible native model input, 2026-10-22

This change belongs only to `codex/native-model-runtime`. It implements the
revised compatibility contract dated 2026-10-22, rather than the earlier proposal
to remove every check or resource limit. The review baseline was CPA
`65f134152ad9696b1568b0372f57af02fad4eaa3` and codex-server
`f18f16f5b3d5a76c3f8fe56a1863507d88b62bdb`. The official comparison version
remains Codex 0.162.1 (`092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`).

## Ownership

CPA translates the public protocol, maps the model, applies its configured
business processing and final payload rules. Its existing translation envelope
and plugin hooks run before those rules. A request-local translation context
distinguishes native business input from the traditional direct HTTP payload;
it is not a product option or an execution bypass.

The worker normalizes the remaining input shape, maps it into native prompt,
tool, history and generation representations, and uses the native model sender.
There is no raw final-body overlay, separate bridge sender or CPA direct-egress
fallback. Native transport, authentication, routing metadata, completion and
stream processing remain with that sender. Retry budgets remain zero under the
existing user decision; this change does not restore automatic native retries.

Model events return through CPA while generation is in progress. Function/custom
tools, including tools named `Read`, `Bash`, `Agent`, `WebSearch` or `ToolSearch`,
are caller declarations. Their names do not install or select server executors.
Tool-only completion is a successful model invocation, not an Agent loop.

## Input adaptation

- Responses, Chat, Claude, Gemini and Interactions converters retain caller tool
  names and complete schemas at the Runtime business-input boundary. Nested
  functions, `input_schema` and supported source aliases normalize before the
  existing SDK hooks. Pattern, union, numeric/array constraints, dialect keys and
  explicit output schemas are not rewritten to fit a narrower tool template.
  Gemini source type enums normalize only at schema nodes; case-sensitive
  `default`, `enum` and `const` values and large numeric constraints stay intact.
- Chat, Claude and Gemini non-strict function defaults remain non-strict.
  Missing reasoning effort or include does not acquire a hard-coded Agent
  template in those CPA converters. The native layer owns its remaining defaults.
- Explicit `none`, `false` and empty lists remain distinct from omission.
  `tool_choice`, `include`, parallelism, structured-output names/strictness,
  service tier and mapped generation controls enter native input representations.
  Public `fast` is normalized to `priority`; CPA does not delete other explicit
  tiers before native capability mapping.
- Chat tool results with a missing ID are paired only when one outstanding call
  makes the relationship unambiguous. Duplicate IDs, orphan results and missing
  required names produce field-specific errors before a lossy source conversion.
  Worker-side history adaptation handles native input relationships separately.
- Display metadata is consumed without requiring DTO round-trip equality.
  Authentication, device information, timing and Lite transport mirrors belong
  to the worker's current session, not stale caller values.
- Nested `client_metadata.session_id`, `thread_id` and serialized turn metadata
  participate in CPA session selection. Explicit header identity has precedence.
  Business changes do not replace an explicit source session with a content hash.
- Final payload overrides and filters run after CPA adaptation. Subsequent native
  construction may normalize shape and create native transport information, but
  must retain configured business meaning. Tests compare the relevant semantics,
  not whole request JSON equality as an admission rule.

## Images and stateful controls

Standalone image generation/editing still uses the worker's native Images
backend. The native typed options now carry mask references, output format,
compression, partial images and stream mode. CPA adapts multipart image/mask
files to the same business references as JSON. `stream:false` does not discard
`partial_images`. JSON and SSE responses follow their actual content type;
partial events are delivered before native image completion.

Native support does not guarantee that a live upstream model accepts every
parameter combination. Unsupported business content is reported by field;
optional metadata is not a blanket rejection reason.

`generate:false` is native WebSocket prewarming. An HTTP-only native model path
reports that concrete capability gap without generating a response instead.
`store` does not create a local conversation service. Background retrieval and
unmaterialized stored conversations remain unimplemented; supplied history and
existing downstream WebSocket continuations remain supported. Standalone legacy
`/responses/compact` stays rejected; ordinary compaction history and triggers stay
on the model path. Token counting and per-request proxy overrides are separate
capabilities, not implemented by creating another sender.

This 0.162.1 entry has no native Responses mapping for `top_k` or
`prompt_cache_options`. CPA retains those controls so the worker reports the
specific gap, rather than silently deleting them or pretending they took effect.

## Notification and diagnostic handling

Optional RPC notifications are dispatched without the former 128-notification
failure threshold. Only actual business event, error and completion frames
control inference. Their required request identity and payload parsing still
apply. Malformed, unknown or mismatched optional diagnostics are logged and
ignored; they neither fail the model nor contaminate another request's log.
Safety-buffering response headers are retained with the existing safe header
projection. Credentials and raw routing cookies are not exposed to clients.

Paired upgrades provide the current input capabilities. A worker can reject an
unsupported option with its concrete error; CPA never switches to direct model
egress. Compatibility uses actual business notification, completion, diagnostic
and transport capabilities rather than a private protocol number.

## Disposition of the original 81 observations

The IDs below retain the revised report's classification. “Separate” means this
input-compatibility change does not remove the existing lifecycle or observability
policy; it does not mean an unlimited-capacity implementation was delivered.

| ID | Disposition in this revision |
| --- | --- |
| A01 | Map native tool selections; remove auto-only admission. |
| A02 | Consume/rebuild transport metadata; retain usable source provenance. |
| A03 | Map supported business controls; identify actual unmapped business fields. |
| A04 | Preserve explicit include lists, including empty lists. |
| A05 | Map native reasoning controls; consume classified optional metadata. |
| A06 | Map native text controls; consume classified optional metadata. |
| A07 | Map native plain, JSON-object and schema output formats. |
| A08 | Preserve caller output-schema names; default only when omitted. |
| A09 | Normalize history shorthand; retain errors for unknown business items. |
| A10 | Remove generic JSON round-trip equality as an admission condition. |
| A11 | Consume output-only status/annotations without deleting actual content. |
| A12 | Map store; explain unsupported asynchronous background retrieval. |
| A13 | Preserve generate semantics; require real reconstruction for stored history. |
| A14 | Normalize tool/choice/namespace wrappers by their declared meaning. |
| A15 | Keep native tool-type capabilities distinct from arbitrary caller names. |
| A16 | Preserve explicitly supplied function output schemas. |
| A17 | Keep unambiguous names, duplicate-name and selection relationships. |
| A18 | Map explicit context and verbosity instead of requiring derived equality. |
| A19 | Regenerate native Lite transport mirrors. |
| A20 | Map active native Images options, including mask/output/partial/stream. |
| A21 | Use credential-scoped internal identity mapping. |
| A22 | Use native internal identity representations. |
| A23 | Normalize source provenance rather than require a caller-native enum. |
| A24 | Token counting remains a separate, explicit capability gap. |
| B01 | Preserve the existing user-selected zero-retry policy. |
| B02 | Preserve the existing single-send recovery boundary. |
| B03 | Do not introduce transport retries in this input revision. |
| B04 | Images retain zero retries while using native live delivery. |
| B05 | Retain native per-thread connection/cache leasing. |
| C01 | Separate explicit business values, source defaults and internal streaming. |
| C02 | Preserve explicit include; leave remaining defaults to native ownership. |
| C03 | Retain explicit tiers for native mapping; normalize the fast alias. |
| C04 | Preserve generation controls for mapping instead of bulk deletion. |
| C05 | Preserve state controls until their actual history/capability handling. |
| C06 | CPA consumes public usage delivery; native maps its stream controls. |
| C07 | Do not compare client declarations with bundled tool templates. |
| C08 | Do not invent/replace tools; retain explicit hosted-image operator policy. |
| C09 | Do not force Lite parallelism over an explicit boolean. |
| C10 | Preserve caller schema constraints before final payload configuration. |
| C11 | Preserve opaque reasoning state for native handling; do not guess its format. |
| C12 | Keep existing image capability routing; no unlimited-model claim. |
| D01 | Separate context capacity/lifecycle assessment. |
| D02 | Separate context/cache expiry assessment. |
| D03 | Remove notification-count failure; dispatch by responsibility. |
| D04 | Keep startup lifecycle distinct from model generation timeouts. |
| D05 | Separate replay-cache capacity from required business history. |
| D06 | Separate replay-cache eviction from required business history. |
| D07 | Do not mix traditional OAuth ticket policy into native input changes. |
| D08 | Do not mix the traditional SSE scanner into native bridge changes. |
| E01 | Keep implemented source/response protocol dispatch. |
| E02 | Accept downstream steering intent; native owns upstream transport. |
| E03 | Keep network configuration in native ownership; no alternate HTTP client. |
| E04 | Keep real operation/capability negotiation and the compact exception. |
| E05 | Accept compatible optional IPC fields; validate required framing. |
| E06 | Optional diagnostics cannot replace the native business outcome. |
| E07 | Unknown optional notifications do not terminate inference. |
| E08 | Keep actual protocol/execution compatibility checks. |
| E09 | Distinguish adaptation errors from native/upstream errors. |
| F01 | Retain required safe response headers; keep secret filtering. |
| F02 | Separate thread/turn analytics capacity assessment. |
| F03 | Separate analytics tool-count assessment. |
| F04 | Separate diagnostic snapshot size assessment. |
| F05 | Separate diagnostic identity projection assessment. |
| F06 | Separate analytics expiry assessment. |
| F07 | Keep diagnostic body truncation distinct from actual body delivery. |
| F08 | Separate interceptor observation history assessment. |
| F09 | Separate response-model alias rewrite assessment. |
| F10 | Keep diagnostic node parsing independent of model input admission. |
| F11 | Keep time-zone parsing independent of model input admission. |
| F12 | Keep command statistics independent of actual tool output. |
| F13 | Keep internal/native outcome filtering and incremental business events. |
| G01 | Retain the existing complete raw-schema carrier and add chain regressions. |
| G02 | Retain the local bridge without fixed message/frame limits. |
| G03 | Retain dynamic notification queues. |
| G04 | Retain valid upstream extension-event delivery. |
| H01 | Keep the user-approved standalone legacy compact exception. |
| H02 | Keep real JSON/native/upstream errors after reasonable adaptation. |
| H03 | Keep credential ownership and disabled-account semantics. |
| H04 | Keep cancellation and business request/frame association. |
| H05 | Preserve caller schema constraints; do not execute client tools. |
| H06 | Keep native success confirmation and tool-only model completion. |

## Verification

CPA unit regressions cover complete schemas/names across the five existing
source converters, omitted versus explicit controls, final payload rules,
idempotent normalization, tool association, nested identity, optional notifications
and diagnostics. Real-worker regressions block and count CPA model transport;
the expected count is zero. They inspect the native sender's actual request,
exercise JSON/multipart images with masks and output options, and gate image
generation between partial events to prove incremental client delivery.

Run the paired native tests with the matching worker executable:

```sh
go test -race ./internal/runtime/executor/helps/codexruntime ./sdk/cliproxy/session ./internal/translator/codex/... ./sdk/api/handlers/openai
CODEX_RUNTIME_TEST_BINARY=/path/to/codex-app-server go test -race ./internal/runtime/executor -run '^TestCodexRuntime.*RealWorker$' -count=1 -v
go build -o test-output ./cmd/server && rm test-output
```

These fixtures use synthetic credentials and controlled local services. They do
not demonstrate live ChatGPT acceptance, production prompt-cache hit rates or
long-running production capacity. The ordinary official CLI retains its native
behavior when no model-only request controls are supplied.

### Recorded paired result

The pre-migration worker snapshot reported `codex-app-server 0.162.1`, SHA-256
`bd9509a21b64e7258571e5ec42055e41fadacf0f4a2add88ee99c2bc32490adb`.

| Check | Result |
| --- | --- |
| Real master/worker HTTP, WebSocket and handshake matrix with Go race detection | 115 model scenarios / 199 leaf checks passed |
| CPA model transport inside those scenarios | Zero model requests |
| Images JSON/multipart mask, output options, partial images, stream false/true | Passed on the actual worker sender |
| Gated Images delivery | Both partial events arrived before allowing the next upstream phase; first event about 6–10 ms locally |
| Existing live client tools, native continuation/cache, compaction, tickets, gateway logs, failures and cancellation | Passed in the paired matrix |
| Executor, helpers, bridge, session and all Codex translator packages with race detection | Passed |
| Runtime/native API-handler regressions with race detection | Passed |
| Complete API-handler package without race detection | Passed |
| Required CPA server build, Go formatting and whitespace checks | Passed |

A wider API-handler race run exposed an existing traditional direct-WebSocket
race in `TestCodexWebsocketTopicSurvivesDownstreamReconnect/duplex=true`.
`gin.Context.reset` races with the old duplex connection's deferred
`CodexTurnState.LogResponse` update. The same stack was independently reproduced
from an untouched Git archive of baseline `65f13415` using:

```sh
go test -race ./sdk/api/handlers/openai -run '^TestCodexWebsocketTopicSurvivesDownstreamReconnect$' -count=50 -cpu=1,2
```

This revision does not change that legacy duplex/logger code. The broader race
suite is not reported as passing, and that separate race is not included in the
compatibility fix. Repeated whole-package baseline tests also expose an unrelated
model-registry fixture reset issue in `TestCodexClientModelsApplyPatchRouting`;
it is not used as evidence of this revision's correctness.

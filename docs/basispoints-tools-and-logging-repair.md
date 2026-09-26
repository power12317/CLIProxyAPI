# Basispoints tools and logging repair — 2026-09-25

## Function encryption metadata and temporary 403 fallback — 2026-09-26

Function calls decoded from the Basispoints transport now explicitly declare
`encrypted_function_args: []`. Missing and empty metadata are different to Codex
collaboration clients. Direct function calls retain their upstream encryption
declaration, and genuinely encrypted arguments are not renamed by the native
plan adapter. Wrapper encryption metadata remains in native replay only; it
does not describe the decoded client arguments. SSE item events and the
completed/non-streaming response carry the same declaration. Existing reasoning
ciphertext, agent messages, custom input and account rotation stay intact.

An upstream Basispoints HTTP 403 pauses the protocol globally within the running
CPA process for 30 minutes, including attachment-upload rejections. The current
request retains its error and is not replayed. Subsequent requests use the
existing native Codex HTTP/WebSocket selection while paused. Other HTTP statuses
do not start this pause. Concurrent rejections do not extend an active window.
The warning log includes the request ID and the automatic recovery time.

The pause is separate from `codex.basispoints.enabled` and survives executor
replacement during configuration reloads. Once the deadline passes, new
requests use Basispoints only if that setting is still enabled. A manual disable
therefore remains authoritative. The pause is in-memory and resets on process
restart; no config file, credential state, account binding or timer callback
enables Basispoints.

Regression coverage uses a controlled clock for expiry, concurrent 403s,
configuration reload, manual disable and account rotation. It also covers
plaintext collaboration calls and direct encryption declarations across response
modes; it does not claim live upstream decryption or policy acceptance.

## Handoff and custom exec compatibility — v2026.09.25-3

Two supplied request logs contained a Codex cross-thread handoff represented as a
function result with a codex_app/create_thread identity and no call_id. At the
Basispoints serialization boundary, recognized idless create_thread and
send_message_to_thread delegation outputs now become source-labelled user context
with their complete output preserved in place. They do not require a subagent
header, enter the native-call cache, or count as a tool iteration. Paired and
unrelated tool results retain the existing replay behavior.

The third log returned a cmd/workdir argument object for the custom functions.exec
tool. That JavaScript client tool now receives a program invoking its own
tools.exec_command with every original parameter serialized as data. CPA does not
execute the program. Raw input/args/arguments string aliases remain exact; other
custom tools do not receive this command-object adaptation. The upstream tool
catalog includes a concrete JavaScript example and distinguishes exec from
exec_command. Native replay items remain intact and account rotation still works.

Validation replays all three supplied payloads offline, covers cold/warm cache and
another selected account, verifies SSE and non-streaming executor delivery, and
executes the generated JavaScript against a stub that checks all original command
parameters without invoking a shell. Regression fixtures contain synthetic data.
HTTP/SSE commit timing, models, reasoning, suffixes and account selection are not
changed. No live OAuth generation is claimed by these checks.

Reference: [ranxi2001/sub2api, d215eddd](https://github.com/ranxi2001/sub2api/tree/d215eddd9831cbf9933aaca6c89d6bf13bb561ec/backend/internal/service/basispoints).
The reference converts client tools into the Excel transport; it does not send
arbitrary client declarations as native Basispoints tools. Its complete catalog,
raw custom transport, direct calls, history recovery and terminal-item handling
exposed gaps in the previous CPA implementation.

## Scope explicitly requested by the user

- Remove all Basispoints-added account binding. The original binding lines were
  taken from Git commit `3365498f` and removed with a Git-generated reverse patch:
  73 lines across the adapter, replay ownership and auth transport selection.
  Existing native scheduling stays in place. No session, reasoning or tool owner
  is installed as a selected-credential constraint.
- Restore random eight-character hexadecimal request IDs. `requestid.go` is
  restored directly from Git commit `e76ba0ed`; its blob hash is identical.
- Preserve the complete client tool contract and restore native Codex logging.
- The user explicitly chose native Codex routing for hosted tools that cannot
  run through Basispoints. No tool declaration is silently removed to obtain a
  successful request. No model-name or reasoning-effort remapping is added.

## Protocol changes

- Include `input.additional_tools`, nested namespaces, complete parameters,
  `inputSchema` aliases, custom `format`/grammar and strict-schema information
  in the client tool catalog. Duplicate identical declarations are accepted.
- Accept the existing `tool/args` envelope and the reference `name/arguments`
  envelope. Custom tools also use an explicit `cpa.custom/<catalog name>` marker
  with exact raw input in `code`; the reference marker remains accepted.
- Convert direct declared client calls and the native plan representation back
  to the client's declared tool. Tool code is only relayed, never executed by CPA.
- Rebuild complete client-provided tool history after cache loss or account
  rotation. Cache entries remain isolated without influencing account selection.
- Give calls and tool results distinct, deterministic item IDs. Wait for complete
  terminal tool items while still forwarding text immediately; partial native
  envelopes are not treated as completed client calls.
- Route hosted tool declarations, including additional/namespaced declarations,
  through the existing native Codex executor with all declarations retained.
  This route is selected before sending, not after an upstream error.

## Logging

Basispoints now attaches the existing Codex access-log state: auth filename,
session ID, turn ID, requested/returned model and observed ticket lengths. Client
identifiers are retained when present; generated upstream task/turn identifiers
are used only as logging fallbacks. Actual ticket lengths are observed from the
wire. Missing Basispoints ticket headers produce zero lengths; no ticket is
invented or injected to populate a log column.

Raw upstream events remain available to the existing request logger, and 422
diagnostics include status, effort, tier, input-item count and upstream request ID.
They do not print prompts, tool arguments, tokens or encrypted reasoning.

## Verification

Regression tests cover cross-account tool continuation, complete custom input and
additional tools, stable catalogs, direct/plan calls, terminal tool completion,
distinct replay IDs, hosted-tool routing, and formatted Gin logs for JSON/SSE
success and 422 rejection. The request-ID restoration is checked against its Git
blob. Full Go tests, the server build and targeted race suites are required before
publishing `basispoints/v2026.09.25-6`.

Live account generation is not exercised: this checkout contains no OAuth
credentials. The tests use controlled HTTP/SSE upstream responses and actual CPA
execution/logging paths; they do not claim to prove every upstream 422 resolved.

## Tool envelope follow-up — basispoints/v2026.09.25-7

The local `invalid_tool_envelope` error exposed gaps in response decoding rather
than an upstream HTTP 422. Regression cases reproduced failures with prose-prefixed
JSON, CRLF/case-varied JSON fences, literal line breaks and illegal JSON escapes,
explicit client-call wrappers, and nested raw-custom markers.

The decoder now handles those formatting variants. Valid JSON is decoded first
with exact numeric precision. Only failed decoding invokes string repair; valid
escapes retain their existing meanings. Marked custom input bypasses JSON recovery
at every supported wrapper level, preserving its raw content and the original
cached replay item. Explicitly named single-call wrappers can recover a declared
tool envelope or function arguments without executing their surrounding text.

Errors retain the existing code and add the parsing stage, format, byte length,
and JSON error category/offset where available, without exposing argument content.
Truncated JSON, multiple calls, and raw code with no identifiable tool continue to
return an error. The prompt explicitly reiterates the custom marker and JSON
escaping rules. Account selection, hosted-tool routing, model names, reasoning
suffixes, service tiers, and CPAMP remain unchanged by this follow-up.

Verification includes before/after reproductions, full Go tests, focused race
tests, and the required server build. Real upstream account generation is not
part of these local regressions.

## Images, Astra max and complete error logs — basispoints/v2026.09.25-8

The explicitly requested Astra max adapter is inside Basispoints serialization:
only `gpt-6-astra` with canonical effort `max` uses wire effort `xhigh` and prepends
`{"type":"configuration_update","reasoning":{"effort":"max"}}` to input.
This is a configuration item, not a prompt instruction. Other efforts/models,
native Codex routing and the existing suffix parser remain unchanged.

User-message inline images upload directly to the Basispoints attachments
endpoint as multipart field `file`, using the selected account's authentication
and proxy transport. The returned `openai_file_id` replaces `image_url` with
`file_id`. Exact image bytes and explicit detail are preserved. Tool-result
images keep their native inline representation. The cache holds only digests
and file IDs, does not pin accounts, and reuploads when another account is
selected. Upload errors are returned; images are never omitted to obtain success.
No public image server or new configuration field is required.

The parser additionally accepts one complete declared tool envelope inside an
assignment or surrounding prose. This is deterministic parsing, not JavaScript
execution or another generation attempt. No automatic tool-correction retry,
image omission, effort downgrade retry, or account binding is introduced.

Input rejection and response-conversion failures promote only the failed request
to the existing error-log path. With `request-log: false`, `error-*.log` contains
the complete original client body, complete generated upstream body, failing
upstream payload/event, parsing phase, and decoded tool arguments/code. These
failure snapshots do not use the normal 32 MiB deferred-capture cap. Streaming
failures are logged even after downstream HTTP 200. Successes retain the normal
logging policy. Authorization headers are not added to the diagnostic snapshot;
normal header masking and the existing global commercial-mode logging disable
remain in effect.

Tests cover actual local HTTP multipart upload and response submission, JSON/SSE
paths, cache reuse and account rotation, native-path isolation, suffix precedence,
the exact max configuration item, malformed tool events, no correction retries,
logging with request-log on/off, and bodies exceeding 32 MiB. They do not claim
to validate live upstream account behavior.

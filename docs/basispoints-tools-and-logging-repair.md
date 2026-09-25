# Basispoints tools and logging repair — 2026-09-25

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

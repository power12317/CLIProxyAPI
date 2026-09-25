# Basispoints request contract repair — 2026-09-25

The previous adapter modified the incoming Responses body in place and removed
only a short list of fields. Both reference adapters instead construct a separate
Basispoints envelope. As a result, ordinary Codex fields such as `include`, `text`,
`max_output_tokens`, `type`, and `prompt_cache_retention` could leak into the
Basispoints request. Null context policies and unnormalized history/metadata
were also forwarded.

## Service tier audit

No production request builder was found synthesizing `service_tier: "default"`
when the caller omitted it. Two paths nevertheless allowed it onto the wire:

1. `PreserveCodexProtocolFields` copied an explicit client `default` back after
   the Codex translator had removed it. Its tests previously required this behavior.
2. The Basispoints adapter bypassed that translator and retained the incoming
   tier unchanged. Payload overrides could also reintroduce an ordinary tier
   after the native translation stage.

The repaired shared wire rule emits only explicit accelerated tiers: `fast`
becomes `priority`; `priority` and `ultrafast` are retained. All other values,
including omitted, null, empty, `default`, `auto`, and `standard`, result in an
absent field. The native preservation function does not read or copy the original
`service_tier` at all; it only retains the translator's result. Native
HTTP, SSE, WebSocket, compact, and image execution normalize again after payload
overrides. Basispoints uses the same rule when constructing its envelope.

Usage accounting has its own `DefaultServiceTier` label. It describes reporting
semantics and is not a write to the upstream JSON body; that reporting behavior
is unchanged. Upstream response tiers are also retained as response data.

Explicit accelerated requests are not silently downgraded or retried without
the field. A Basispoints rejection retains its upstream status and error body.

## Basispoints envelope and history

- Construct `model`, `model_selection`, `stream`, `store`, `input`, and `metadata`
  explicitly. Preserve the original model and the canonical pipeline's actual
  `reasoning_effort`, without inserting an effort when none was supplied.
- Copy only the supported optional cache key, context policy, and explicit
  accelerated tier. Omit null/empty context policies while forwarding explicit
  nonempty policies for upstream validation.
- Encode scalar metadata as bounded strings; omit nested objects, arrays, and
  nulls. Preserve integer precision and Unicode boundaries.
- Replay encrypted reasoning as `type`, empty `summary`, and `encrypted_content`.
  Omit bare reasoning and stored-item references in the stateless request.
- Preserve complete cached native tool calls and use deterministic function IDs
  for replayed tool results. Tool output content and caller/account isolation
  remain unchanged.

The existing thinking suffix parser, model routing, ticket probe settings and
CPAMP controls are unchanged.

## Verification

The new request tests first failed on the old implementation, exposing leaked
wire fields, ordinary tiers, null policies, reasoning references, and metadata
types. Strict serialized-request tests then check Codex and Responses inputs,
streaming and nonstreaming execution, ordinary mode, Fast and Ultra, and the
unchanged propagation of an upstream 422. Existing native HTTP/SSE/WebSocket/
compact tests now require ordinary tiers to be absent, including after payload
overrides. Tool replay tests remain in the suite. Subsequent user-directed repair
removes Basispoints account pinning and tests ordinary account rotation instead.

Reference sources:

- [cpa-plugin-oai-basispoints protocol.go, 708082da](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/708082da2f851569984de395d25405e61c2bbc34/internal/basispoints/protocol.go)
- [ghcp_proxy excel_upstream.py, 1a73157d](https://github.com/Nonary/ghcp_proxy/blob/1a73157d579dcdaa08d8ecbcd166e80ca48c9e66/excel_upstream.py)
- [Reference issue documenting default/priority 422 responses](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/issues/3)

This repair follows the wire shapes, not the reference model aliases or effort
fallbacks. The request contract is validated with a local strict upstream fixture;
no real OAuth credentials are available in this checkout for a live account test.

Validation completed: `go test -p 4 ./...`, the required server build, targeted
race suites for the adapter/executors/OpenAI protocol handlers/auth manager,
and the Docker workflow's actionlint check. Publish this repair on the existing
Basispoints branch with the immutable `basispoints/v2026.09.25-5` tag. CPAMP does
not need another build for this backend-only change.

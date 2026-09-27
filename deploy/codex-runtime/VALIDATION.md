# Plan B validation record

> Historical test results for v1, not validation of the current v3 master design.
> The current contract is [PROTOCOL_V3.md](PROTOCOL_V3.md); current deployment is
> documented in [README_CN.md](README_CN.md).

Date: 2026-09-27. CPA branch: `codex/plan-b-managed-auth`, based on `02a86349`.
CPA implementation commit: `1853b364`.
Protocol: `cpa/*` version 1. Codex fork branch: `codex/cpa-managed-auth`.
Final Codex fork commit: `8ee538eb3f27e8b2b560a9dc9c4372b5d805748a`, following
the independently reviewable raw Responses transport commit `793e731cc`.
Codex upstream baseline reported by the fork: `985cf47a4eb6084b2ff6b30ebdb1216acda85bb4`.

## CPA checks

| Check | Result |
| --- | --- |
| `go test ./...` on the final code | PASS; 100 packages reported `ok`, other packages have no tests |
| `go build -o test-output ./cmd/server && rm test-output` | PASS |
| Targeted `go test -race` for runtime client/executor, auth manager, service registration, and API bootstrap | PASS |
| Runtime configuration, token-free synthesis, config removal/disable/re-enable, invalid hot reload | PASS |
| Auth refresh scheduling, forced refresh, persistence and management mutation boundaries | PASS |
| Request/response fidelity, unknown stream fields, caller isolation, protocol/account mismatch | PASS |
| Tool-call adaptation to OpenAI Chat and Claude | PASS |
| No replay after ambiguous disconnect, including API bootstrap and configured continue rules | PASS |
| Merged Compose configuration and network/volume assertions | PASS; static validation only |
| `git diff --check` and formatting of changed/new Go files | PASS |

The initial executor package run exposed a timing failure in the existing
`TestCodexWebsockets_KeepalivePingDuringUpload_NonstreamSessionless`; its targeted
rerun and both subsequent full-suite runs passed. No unrelated WebSocket code
was changed to suppress it.

The requested `gofmt -w .` encountered read-only files inside an ignored nested
module cache under `deploy/codex-turn-state/.cache/mod`. All changed/new Go files
were formatted and checked separately; incidental formatting of two unrelated
tracked tests was reverted.

## Actual fork integration

Ran `TestCodexRuntimeForkContract` with the built macOS arm64 app-server at:

`/Users/mac/Documents/www/codex/codex-rs/target/debug/codex-app-server`

Tested binary SHA-256:

`89e428caa9edafa4ac108c8479f1a1299746719c9e2921523d021f11032c3eb6`

The final binary was verified and `TestCodexRuntimeForkContract` passed again
after the fork's final formatting/lint/build. Its final changes included safe
error classification, lazy provider creation while the bridge is disabled, and
additional cancellation during connection draining. The earlier development
artifact was also tested, but the checksum above identifies the final verified
artifact (431,367,696 bytes).

This is a development artifact checksum, not a published release or container
digest. Rebuilds require rerunning the contract test; use immutable image digests
only after validating deployment artifacts.

The test starts the real fork process with an isolated temporary HOME and
CODEX_HOME, synthetic managed OAuth credentials, and local HTTP fixtures. It
verified:

- App-server initialize/capabilities and expected managed account binding.
- Official unauthorized recovery: bounded old-token retries, one refresh request,
  and rotated credentials written by Codex to its own native storage.
- One accepted inference for each successful request, with no automatic tool
  continuation; complete tool call and unknown native request fields preserved.
- Full terminal response collection and raw streaming extension events.
- Unsupported client identity metadata rejected before any upstream inference.
- HTTP 503 submitted exactly once, without upstream or CPA replay.
- Downstream cancellation closes the active upstream request, while a subsequent
  capabilities connection proves the runtime survives.

## Remaining deployment acceptance

No real ChatGPT login or paid inference was performed. Linux container builds,
actual IPv6/proxy reachability, persistent volume ownership, real-account login,
and long-running production lifecycle behavior need operator validation. The
Compose files were rendered and checked, not started against the user's running
deployment. Plan A remains unimplemented on its independent branch base.

The dedicated Codex project chat reports 10/10 CPA bridge acceptance tests and
665/665 configuration/protocol tests passing. Its four TUI color assertion
failures passed when rerun without the inherited `NO_COLOR` setting. Its broader
app-server/API retry set still contained 27 failures and one timeout at this
checkpoint; these have not all been established as baseline or environment
failures. Therefore the Codex-wide regression suite is **not** recorded as clean.
The fork reports successful required `just fix`, `just fmt`, and final
`cargo build --locked -p codex-app-server --bin codex-app-server`. Per its local
instructions, the Rust tests were not rerun after final fix/format. The CPA-run
binary integration test above did run against the final artifact. The detailed
failure list is in the fork's `codex-rs/app-server/docs/cpa-bridge-validation.md`.
These reported Rust results are distinct from the actual CPA-run binary test.

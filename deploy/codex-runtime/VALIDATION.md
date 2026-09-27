# Plan B validation record

Date: 2026-09-27. CPA branch: `codex/plan-b-managed-auth`, based on `02a86349`.
Protocol: `cpa/*` version 1. Codex fork branch: `codex/cpa-managed-auth`.
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

`574ec7d7013006555d303a0e7599ae85ac5fdc05875b44899215ba31f0c5da51`

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
deployment. Codex repository unit/integration/build results are maintained by
its dedicated project chat; this record covers CPA and the concrete cross-repo
artifact test only. Plan A remains unimplemented on its independent branch base.

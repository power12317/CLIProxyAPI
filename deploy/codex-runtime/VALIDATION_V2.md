# Shared-credential v2 validation

Date: 2026-09-27. CPA branch: codex/plan-b-managed-auth, following v1 commit
12963bd1. This record supersedes v1 deployment instructions, not its historical
test results.

## Matched repositories

- Codex branch codex/cpa-managed-auth, commit
  60942804a9e843a0dd5ad17ca1781310f231d490.
- CPAMP branch codex/codex-runtime-control, commit
  7e15191217e58692da7510bf31daabbf3d667aad.
- CPA implementation and this record are saved together on the CPA branch.

The final macOS arm64 Codex app-server artifact is 431,788,480 bytes. SHA-256:

`ed8f0f0e59f541f35a32031ab49ae5f8d616975e30dbe3b08ee0ca85d4f34bd8`

## CPA checks

- Full `go test ./...`: PASS (100 packages report ok).
- Required server build and removal of test-output: PASS.
- Targeted race tests for management, executor, shared client, file store, auth
  manager and service: PASS.
- Disabled configuration/status performs no worker RPC: PASS.
- Master/per-file switching, saved preference retention, worker removal and
  standalone configuration restoring CPA ownership: PASS.
- Native CPA reads the worker's latest access/refresh tokens, refreshes them, and
  the worker then reads CPA's updated token from the same file: PASS.
- Ordinary CPA status persistence retains the shared token values and metadata:
  PASS.
- Simplified Compose renders with the shared network/auth directory, no socket
  volume, no command list, and no published worker port: PASS (static only).
- Formatting of modified/new Go files and git diff --check: PASS. The repository
  wide gofmt command encountered the existing read-only ignored module cache;
  its incidental formatting of two unrelated tracked tests was reverted.

## Actual cross-repository test

`TestCodexRuntimeForkV2OAuthAndModeSwitch` passed against the final checksum above
after the Codex final lint/format/build. It starts the actual app-server with a
temporary HOME/CODEX_HOME, fixed shared CPA credential path, authenticated TCP
WebSocket, and local fake token/Responses endpoints. It exercises real CPA
management handlers and executors, not just a JSON-RPC mock:

1. Turn on the master switch and obtain the official authorization URL through
   the CPA management endpoint.
2. Submit the complete callback URL on another connection; Codex performs PKCE
   code exchange and saves the designated CPA file.
3. Execute inference through Codex and trigger native 401 recovery/refresh;
   verify the new refresh token appears in that same file without auth.json.
4. Turn off the integration and execute through native CPA with the worker's
   updated access token.
5. Turn it on again and verify full raw streaming events and continued runtime
   availability.

Reproduce with CODEX_RUNTIME_TEST_BINARY set to the fork app-server and:

```sh
go test ./internal/api/handlers/management -run '^TestCodexRuntimeForkV2' -count=1 -v
```

## Other project results

The Codex topic reports 714 login/transport/protocol/CPA tests plus four raw-event
tests passing, scoped clippy/fix and formatting completed, and a successful final
locked build. The earlier broad v1 regression's 27 failures and one timeout remain
documented in the Codex repository; no claim is made that its entire upstream
test suite is green.

The CPAMP topic reports 4,323 frontend tests, 260 repository tests, backend tests,
proxy race tests, type checking and the single-file panel build passing. Lint has
only its six existing warnings. Its isolated browser mock exercised the URL and
manual callback UI. Existing user deletion of docs/CPA_CODEX_SYSTEM_OAUTH_HANDOFF.md
was retained outside its 16-file feature commit.

## Deliberate scope

No request-count feature, cross-process lock, lease, epoch, CAS, refresh mutex or
handoff protocol was added. The user explicitly declined extra protective
mechanisms; concurrent refresh and mode switching are not strictly serialized.
Existing ordinary in-process bookkeeping and official OAuth behavior remain.

No real account credentials or paid inference were used. Linux container images
were not built (the local Docker daemon did not respond in the Codex task), no
images/branches were pushed, and no server deployment was performed. Actual
server IPv6 reachability and real-account authorization remain deployment tests.

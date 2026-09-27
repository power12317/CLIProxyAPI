# Plan B master protocol v3 validation

This record applies to the `V3-SEP-20260927` master/account-process design. It
supersedes the v1/v2 validation records for current deployment decisions.

## CPA checks

- `go test ./...`: passed (100 tested packages; remaining packages have no tests).
- `go build -o test-output ./cmd/server && rm test-output`: passed.
- Focused race checks for the master client, management controls, executor,
  shared credential switching, and SDK refresh ownership: passed.
- `docker compose config --format json` for the generic deployment
  overlay: passed. No master port publication, per-account configuration,
  bridge key, or CPAMP service is present.

The regressions cover original credential IDs (case, spaces, Unicode and relative
paths), multiple credentials on one master endpoint, raw HTTP/SSE bytes, split
UTF-8, multiline SSE data, function-call output returned without execution,
existing protocol translation, actual upstream request and response logs,
401/error details, cancellation, and incomplete responses.

Control tests cover global and individual mode changes, direct file flag edits,
latest-token reuse after switching back to CPA, normal credential management,
CPA refresh suppression/restoration, and startup reconciliation before auth load.
A failed master control connection does not prevent already-saved local CPA
configuration from being applied. No refresh locking or automatic replay was added.

OAuth race checks were run in separate groups: runtime OAuth, existing native
Codex OAuth, and generic callback/status handlers. Each group passed. A combined
race run exposed interference between the existing native waiting goroutine and
tests that replace the global OAuth session store; that test-fixture behavior was
not expanded into production synchronization changes.

## Real fork integration

The real-process integration test passed on 2026-09-27 against the local v3 fork
binary. It runs an actual fork master and account child
process against a local fake OAuth issuer and Responses server:

```sh
CODEX_RUNTIME_TEST_BINARY=/path/to/codex-app-server \
  go test ./internal/api/handlers/management \
  -run '^TestCodexRuntimeForkV3OAuthAndModeSwitch$' -count=1 -v -timeout=120s
```

It exercises the existing management OAuth URL, callback and status endpoints
with a tokenless original file, `auth_index` targeting, preserved `client_system`,
native PKCE OAuth, first inference,
401 recovery, shared-file refresh, real request/response log propagation,
function-call response delivery, global disable with CPA native execution, and
reenable with streaming through the account process. The test uses fake tokens
and a local upstream; it is not a live ChatGPT account acceptance test.

## Publication

The development workflows build Linux amd64 and arm64 images. Delivery confirms
branch pushes and Actions triggers; it does not wait for image publication.
Use matching v3 images after their builds publish. CPAMP remains independently
deployed and connects only to CPA.

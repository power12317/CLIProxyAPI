# Codex runtime interface validation

The current extension contract is [PROTOCOL.md](PROTOCOL.md). Run the bridge and
executor suites, then repeat the real-process tests using an immutable worker
binary built from the matching Codex Server commit:

```sh
go test ./internal/runtime/executor/helps/codexruntime ./internal/runtime/executor
CODEX_RUNTIME_TEST_BINARY=/absolute/path/to/codex-app-server go test ./internal/runtime/executor -run RealWorker -count=1
```

Verify both declared business notification/completion forms, actual capability
errors, full caller schemas, explicit controls, incremental delivery, image
operations, cancellation, raw diagnostics and credential isolation. A native
business terminal and diagnostic terminal have separate lifetimes. The HTTP
sender's EOF alone must not be treated as a successful model response. Product
versions and source revisions are metadata rather than compatibility gates.

Legacy protocol and validation records remain available in Git history.

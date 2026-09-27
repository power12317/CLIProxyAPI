# CPA / Codex master protocol v3

Contract identifier: **V3-SEP-20260927**. This document supersedes the earlier
chat alternatives concerning body notifications.

## Identity and state

`credentialId` is CPA's existing file-backed `Auth.ID`: the case-sensitive path
relative to the shared auth directory on Linux. There is no separate worker ID,
account ID, credential filename setting, or owner field in the bridge contract.
The master uses that same ID for its per-credential process map. Private state
directories may encode/hash it internally without creating another public ID.

The shared file has `codex_cli: {enabled: true|false}`. Only a Codex file with
`enabled: true` and without top-level `disabled: true` runs an account process.
Original file names and locations remain unchanged. A worker has a private,
persistent CODEX_HOME and reads/writes the shared original credential file.

## Connection and control

The default endpoint is `ws://127.0.0.1:38317/cpa/v1/ws`. No bridge key or
Authorization header is required. `initialize` / `initialized` remain.

`cpa/capabilities/read {}` and `cpa/credential/reload {credentialId?: string}`
return master capabilities:

```json
{"protocolVersion":3,"runtimeVersion":"...","upstreamRevision":"...","executionMode":"inference-only","operations":["responses"],"rawBody":true,"manualOAuth":true,"upstreamLogs":true,"upstreamBodyLogs":true}
```

Reload scans one credential or the directory and waits for the corresponding
processes to start/stop. It does not change credential control fields. CPA owns
selection and effective flags; the master only applies them.

## Inference

`cpa/inference/start` takes
`{requestId, credentialId, operation, sourceFormat, sessionId, request}` and returns
`{requestId, statusCode, headers}`. There is no `accountId` parameter. The worker
maps the semantic request into the native Codex request builder, sends once
through the native auth/provider transport, and does not start an agent/tool loop.

Ordered notifications:

- `cpa/inference/upstream`: existing `request`, `response`, and `error` log
  notifications with `requestId`, actual URL/method/headers/body/status/message.
  Header values are string arrays. Actual token SHA256 and gateway node remain
  optional logging metadata.
- **`cpa/inference/body {requestId, bodyBase64}`**: original HTTP response bytes.
  This is the only business-response body channel. Do not emit an additional
  `upstream kind:"body"` or parsed `cpa/inference/event` copy.
- `cpa/inference/completed {requestId}`: normal HTTP body EOF.
- `cpa/inference/error {requestId, httpStatus, message, body?, headers?}`: a failed
  transfer; do not follow it with a successful completion.

Send the start result before pulling/transferring body bytes. Request/response
log entries may precede acceptance. CPA logs the raw bytes and applies its normal
SSE/response processing. The master/worker do not parse function-call responses.
Concurrent requests for one credential must remain supported.

## OAuth

- `cpa/auth/login/start {credentialId}` -> `{loginId, authUrl, state}`.
- `cpa/auth/login/callback {loginId, redirectUrl}` -> `{status,error?}`.
- `cpa/auth/login/status {loginId}` -> `{status,error?}`.

The master remembers the login-to-credential association internally. CPA creates
a new UUID-named Codex placeholder file only for an explicitly requested new
authorization; reauthorization always uses the existing ID and original file.
There is no separate independently refreshed auth.json copy.

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

The local bridge imposes no WebSocket frame or message size limit, including
complete inference requests and full upstream diagnostic bodies.

`cpa/capabilities/read {}` and `cpa/credential/reload {credentialId?: string}`
return master capabilities:

```json
{"protocolVersion":3,"runtimeVersion":"...","upstreamRevision":"...","executionMode":"inference-only","operations":["responses","images/generations","images/edits"],"rawBody":true,"manualOAuth":true,"upstreamLogs":true,"upstreamBodyLogs":true}
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

## Independent image operations

The image extension is capability-negotiated within protocol v3. Each image
request uses `operation: "images/generations"` or `"images/edits"` and an
`imageApi` discriminator:

- `images`: CPA's completed Images business JSON. The worker parses native Images
  request types and serializes them through `ImagesClient` to the matching fixed
  endpoint, applying the native image request rules. The worker supplies its own
  authentication and headers.
- `responses`: CPA's image-to-Responses translation. The worker parses native
  input and tools through the same inference builder used for ordinary Responses,
  including its normalization, defaults and model-dependent wire format. CPA
  converts the result and partial image events back into the independent Images
  API response.

CPA accepts JSON and multipart edits. Its existing multipart conversion produces
an ordered `images` array with data URLs and `mask.image_url`, preserving file
bytes and repeated form values. It completes model mapping and other built-in
mutations before applying user payload rules once. The bridge carries the final
CPA business JSON as input to worker normalization. The worker owns the final
upstream representation: it may add required message types, expand shorthand
content and apply native defaults. Do not trim native output to the fields CPA
supplied or overwrite native normalization with the original CPA representation.
Caller credentials and transport identity do not select the worker's credentials,
headers or upstream URL.

Image response bytes use the same body notifications, with JSON or image SSE
according to the actual upstream response. `cpa/inference/completed` means HTTP
body EOF, not a `response.completed` event. CPA recognizes image completion and
error events separately. Closing/cancelling the CPA request cancels the worker
transfer. Actual upstream exchanges retain CPA's request ID in `requestId` and
the worker's `rpcRequestId` diagnostics.

An image operation absent from `operations` is rejected before inference starts.
An unavailable master/worker or failed operation never enables CPA direct model
HTTP as a fallback. Once a managed image request is selected, account retries
must remain restricted to Runtime credentials. Upgrade codex-server first, then
CPA: an older server can continue serving Responses but will reject the new
independent image operations. An older CPA still has the audited image bypass
and is not a valid deployment of this fix.

The independent `responses/compact` operation remains unsupported. Normal
Responses summary requests, `compaction_trigger`, returned `compaction` items,
and the Responses `image_generation` tool retain their existing path.

## OAuth

- `cpa/auth/login/start {credentialId}` -> `{loginId, authUrl, state}`.
- `cpa/auth/login/callback {loginId, redirectUrl}` -> `{status,error?}`.
- `cpa/auth/login/status {loginId}` -> `{status,error?}`.

The master remembers the login-to-credential association internally. CPA creates
a new UUID-named Codex placeholder file only for an explicitly requested new
authorization. After completion, CPA saves the tokens using its standard account
hash, email, plan and client-system filename, removes the UUID placeholder and
reloads the credential worker under the final ID. Reauthorization always uses the
existing ID and original file.
There is no separate independently refreshed auth.json copy.

CPA also sends `cpa/credential/reload` after its file watcher processes Codex
credential additions, changes or deletions. A full rescan handles both identities
when a credential file is manually renamed, stopping the removed worker and
loading the new file without waiting for the master's periodic scan.

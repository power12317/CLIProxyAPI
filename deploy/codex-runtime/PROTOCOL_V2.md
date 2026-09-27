# CPA shared-credential bridge v2

This revision follows the user's approved simplified design. It does not add
cross-process locks, leases, epochs, compare-and-swap, or handoff protocols.
Ordinary owner selection is implemented, but concurrent refresh and mode changes
are not guaranteed to be mutually exclusive. No request-count feature is added.

## Transport

WebSocket JSON-RPC at `ws://127.0.0.1:38317/cpa/v1/ws`, authenticated by a dedicated
Bearer bridge token. Further workers use 38318, 38319, etc. Port 18317 belongs to
CPAMP. Keep initialize/initialized and the inference messages from v1, with
capabilities.protocolVersion=2. The TCP endpoint exposes only bridge methods.
OAuth tokens never travel in bridge messages. Unix v1 refers only to the previous
development artifact; the new shared-file worker uses v2.

## Shared credential file

Each worker reads and writes one configured CPA auth JSON in the shared auth
directory. Preserve the existing top-level Codex token fields and unknown fields.
Do not maintain a second independently refreshed auth.json copy.

```json
{"type":"codex","access_token":"...","refresh_token":"...","id_token":"...","account_id":"...","email":"...","expired":"...","last_refresh":"...","codex_cli":{"enabled":true,"worker_id":"worker-a","owner":"codex"}}
```

`enabled` is the user's per-credential preference. `owner` is the effective owner:
Codex only when the CPA master switch, credential preference, and worker are
enabled; otherwise CPA. The master switch preserves the preference. CPA refreshes
only CPA-owned credentials, and Codex uses/refreshes only its matching Codex-owned
credential. A switch cancels old requests and reloads the same file. File fields
are merged to retain unrelated metadata. There are no ownership epochs or locks.

Capabilities retain the v1 fields and add `credentialFile` (basename), `authOwner`
(`cpa`/`codex`), and `manualOAuth` (boolean). Status capabilities are available while
signed out or CPA-owned; inference and login require the appropriate owner.

## Existing CPA request logs

Workers advertise `upstreamLogs:true`, `upstreamBodyLogs:true` and send ordered `cpa/inference/upstream`
notifications, including before the `cpa/inference/start` result. Each notification
has the matching `requestId` and one of these shapes:

- `kind:"request"`: actual `url`, `method`, `headers` (`string[]` values), `body`
  (string), `accessTokenSha256` and `oaiLbNode` (nullable strings).
- `kind:"response"`: actual `statusCode`, `headers`, `body` (string for an HTTP
  error, null for a successful stream), and nullable `oaiLbNode`.
- `kind:"error"`: transport error `message` when no HTTP response exists.
- `kind:"body"`: `bodyBase64` holds an original successful-response byte chunk.
  CPA reassembles SSE lines before logging, retaining comments, event names, IDs,
  split UTF-8 sequences and the last partial line. These notifications do not
  replace or modify the official response parser or the inference event stream.

Each real upstream attempt gets its own request/response entries, including a
401 followed by token refresh and a second request. Sensitive headers use the
existing masking rules; bearer tokens are not sent over the bridge. Inference
events retain their existing behavior and are not logged a second time when
original body chunks are available.

CPA feeds these entries into its standard request/response log helpers, including
credential, session, turn, requested/served model and turn-state log fields. It
does not generate a second log format or a separate usage pipeline. Structured
RPC failures can carry `data.httpStatus`, `data.body` and `data.headers`; inference
error notifications can also carry `body` and `headers`. CPA preserves upstream
error details and the existing response-header metadata for CPAMP.

## Manual OAuth

- `cpa/auth/login/start {}` -> `{loginId,authUrl,state}`.
- `cpa/auth/login/callback {loginId,redirectUrl}` -> `{status,error?}`.
- `cpa/auth/login/status {loginId}` -> `{status,error?}`.
- `cpa/credential/reload {}` -> capabilities; reload owner/tokens and cancel old requests.

Status values are `pending`, `completed`, and `error`. These methods reuse official
browser OAuth, PKCE, state, and authorization-code exchange. CPAMP displays the
authorization URL and submits the user's full callback URL through CPA. This is
not the device-code flow. Codex writes completed authorization into the configured
CPA file. CPA does not perform the code exchange.

## CPA management API

Paths below are under `/v0/management` and use existing management authentication.

- `GET /codex-runtime`: local-only status, never probes Codex. Returns
  `{enabled,workers:[{id,url,auth_file,token_configured,models,disabled}],credentials:[{name,worker_id,enabled,owner,status,account_id?}]}`.
- `PUT/PATCH /codex-runtime`: `{enabled?,workers?}`. Present workers replaces the
  full list; omitted token preserves the old same-ID secret; empty token clears
  it. Worker input is `{id,url,auth_file,token?,models:[],disabled?}`. Returns full
  local status. Removed/disabled workers return their credentials to CPA.
- `POST /codex-runtime/credentials`: `{name,worker_id,enabled}`; returns full
  status. `name` is the worker's configured auth filename. Preferences may be
  edited while the master switch is off without contacting Codex.
- `POST /codex-runtime/test {worker_id}` -> `{status:"ok",capabilities:{...}}`.
- `POST /codex-runtime/login/start {worker_id}` -> `{login_id,url,state}`. Creates
  the configured file if absent and enables that credential for the login.
- `POST /codex-runtime/login/callback {worker_id,login_id,redirect_url}` ->
  `{status,error?}`.
- `GET /codex-runtime/login/status?worker_id=...&login_id=...` -> `{status,error?}`.

Test/login/status RPC actions require the master switch enabled; otherwise 409
without contacting Codex. Login status may poll Codex only during the explicit
login flow. `/codex-capabilities` advertises `codex_runtime:true`. Older CPA
404/405 results disable the optional UI. CPAMP supports both existing deployments.

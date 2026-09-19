# CLIProxyAPI Codex OAuth Request Fidelity Plan

Status: design only. This document records the implementation plan; it does not change the source code.

## 1. Objective

When an OAuth-backed Codex credential is selected, make the request sent by CLIProxyAPI to `chatgpt.com` follow the observed Codex CLI OAuth request shape as closely as the protocol allows.

The request path covered by this plan is:

```text
API client
  -> new-api
  -> CLIProxyAPI
  -> chatgpt.com/backend-api/codex/responses
```

The OAuth access token and the account ID remain the values belonging to the selected OAuth credential. The API key used by the client to call new-api is only an ingress credential and must never be used as the ChatGPT OAuth credential.

The plan covers:

- OAuth authorization and account identity selection.
- Reconstruction of the Codex CLI application headers.
- Canonicalization of `client_metadata` and nested `x-codex-turn-metadata`.
- Controlled replacement of the installation identity and timezone in environment metadata.
- Windows/Mac detection from the `sandbox` value.
- An account-isolated ChatGPT cookie jar.
- Compression, request logging, response handling, and lifecycle rules.

The plan does not change new-api, implement WebSocket parity, or add a new billing/plan branch. The OAuth HTTP path is the target; API-key Codex behavior remains a separate compatibility path unless a later change explicitly expands the scope.

## 2. Evidence and current implementation

The observed Codex CLI HTTP request uses:

```http
POST /backend-api/codex/responses HTTP/1.1
version: 0.154.0
x-codex-beta-features: remote_compaction_v2
x-codex-window-id: <session-id>:0
x-codex-turn-metadata: { ... }
x-openai-internal-codex-responses-lite: true
x-codex-routing-hint: model=<model>
x-client-request-id: <session-id>
session-id: <session-id>
thread-id: <thread-id>
accept: text/event-stream
content-encoding: zstd
content-type: application/json
authorization: Bearer <OAuth access token>
chatgpt-account-id: <OAuth account_id>
originator: codex-tui
user-agent: <system-specific Codex CLI UA>
cookie: <cookies returned by chatgpt.com>
```

The captured body contains the Codex Responses shape. In the supplied capture, the stable identifiers line up as follows:

```text
prompt_cache_key              = session_id
client_metadata.session_id    = session_id
client_metadata.thread_id     = thread_id
client_metadata.turn_id       = turn_id
client_metadata.x-codex-window-id = session_id + ":0"
x-client-request-id           = session_id
```

The nested `client_metadata.x-codex-turn-metadata` value repeats the same request identity and also contains `sandbox`. The environment text inside the input contains XML such as `<timezone>Asia/Singapore</timezone>`.

The relevant current CLIProxyAPI code is:

- `internal/runtime/executor/codex_executor_execute.go`: translates and normalizes the body, invokes `cacheHelper`, applies headers, sends the request, and translates the response.
- `internal/runtime/executor/codex_executor_request.go`: current `applyCodexHeaders`, `cacheHelper`, identity-confuse logic, instruction normalization, and parallel-tool normalization.
- `internal/runtime/executor/codex_executor_auth.go`: OAuth refresh and `codexCreds` lookup.
- `internal/runtime/executor/helps/utls_client.go`: the uTLS/HTTP2 `http.Client` used for ChatGPT.
- `internal/translator/codex/openai/responses/codex_openai-responses_request.go`: OpenAI Responses to Codex body conversion.

The current HTTP header path has several fidelity gaps:

1. `codexCreds` can prefer `auth.Attributes["api_key"]` before OAuth metadata. That is acceptable for API-key credentials but is the wrong precedence for an OAuth-fidelity request.
2. `Chatgpt-Account-Id` is taken from OAuth metadata, but only when the auth is not classified as API-key auth.
3. HTTP `X-Codex-Beta-Features` is copied from the client only; the OAuth HTTP path does not supply the observed `remote_compaction_v2` default.
4. `X-Codex-Routing-Hint` is not reconstructed from the selected model by the HTTP header builder.
5. Version, window, turn metadata, client request, thread, and session headers are only copied when they arrive from the client; there is no single canonical source of truth tying them to the body.
6. The current fallback `codexUserAgent` is a Mac/iTerm profile and does not equal the requested Windows or Mac UA.
7. The request body is decoded by new-api before it reaches CLIProxyAPI. CLIProxyAPI therefore receives JSON bytes and does not receive the original compressed wire representation.
8. The HTTP executor has no per-OAuth-account ChatGPT cookie jar.

## 3. Required invariants

The implementation must preserve these invariants for every OAuth HTTP request:

| Invariant | Required behavior |
| --- | --- |
| OAuth token | `Authorization: Bearer <auth.Metadata["access_token"]>` for an OAuth auth. The inbound new-api API key cannot override it. |
| Account identity | `Chatgpt-Account-Id` comes from `auth.Metadata["account_id"]`. The account ID is not derived from the API key and is not rewritten. |
| Credential isolation | Token, account ID, cookies, and mutable request state are selected by the same `auth.ID`. No cookie can be reused by another OAuth account. |
| Installation identity | Body metadata, nested turn metadata, and the outgoing installation header use the same derived installation ID. |
| System identity | `sandbox == "windows_elevated"` selects Windows; every other value, including missing or malformed values, selects Mac. |
| User-Agent | The UA is selected from the derived system and is forcibly set on the OAuth HTTP request. An inbound UA, auth custom UA, and old default UA cannot win. |
| Timezone | Only the targeted environment metadata timezone tag is normalized to `<timezone>Asia/Singapore</timezone>`. |
| Prompt preservation | User text, tool arguments, encrypted reasoning content, and unrelated metadata remain byte/semantic equivalent except for the explicitly listed Codex compatibility transforms. |
| Header/body consistency | Session, thread, window, turn, installation, and routing values used in headers are the values represented in the normalized body. |
| Cookie handling | `Set-Cookie` from ChatGPT is stored in the account jar and is automatically sent on later eligible ChatGPT requests. Cookies are never returned to the downstream API client or written to logs. |

## 4. Ordered request pipeline

The implementation should expose the steps as named support concepts rather than placing all logic inline in `Execute` and `ExecuteStream`.

```text
1. Select the OAuth credential.
2. Read and validate the OAuth access token and account_id.
3. Translate the incoming request into the Codex Responses body.
4. Apply the existing Codex compatibility transforms.
5. Read the body client_metadata and nested turn metadata.
6. Determine the system from sandbox.
7. Derive the account installation identity.
8. Rewrite metadata and the targeted environment timezone tag.
9. Resolve session/thread/turn/window/request IDs from one canonical identity.
10. Mark the request as Codex Responses Lite and set the CLI headers.
11. Apply prompt-cache/session handling without changing the canonical installation ID.
12. Zstd-compress the final JSON body for the ChatGPT wire request (enabled by default for OAuth HTTP fidelity).
13. Attach the OAuth account's cookie jar to the uTLS HTTP client.
14. Send the request to /backend-api/codex/responses.
15. Let the client jar consume Set-Cookie, then stream/translate the response.
16. Strip Set-Cookie and other upstream-only headers before returning to the API client.
```

The body rewrite must run after request translation and provider normalization so that the exact bytes sent upstream are the bytes used to build the headers. It must run before `cacheHelper` creates the final `http.Request`. `cacheHelper` should be adjusted so its prompt-cache handling does not undo the canonical installation identity.

## 5. OAuth credential and account selection

Add an explicit OAuth-only credential accessor for the fidelity path, conceptually:

```text
oauthAccessToken(auth) -> auth.Metadata["access_token"]
oauthAccountID(auth)  -> auth.Metadata["account_id"]
```

The accessor must first require an OAuth auth kind (or the existing Codex OAuth metadata shape) and then read the metadata values. It must not fall back to `auth.Attributes["api_key"]` for an OAuth-fidelity request. If the token is missing, return the existing credential error path instead of sending a request with the client API key.

The account ID is used exactly as stored. It is only read for deriving the installation identity; the `Chatgpt-Account-Id` header itself carries the original account ID.

OAuth refresh keeps the existing token refresh behavior. When a refresh changes the access token, the next request uses the new token while retaining the same account-scoped cookie jar and deriving the same installation identity from the unchanged account ID.

## 6. Header reconstruction

### 6.1 Header table

The following table is the target for OAuth `/responses` HTTP requests. Header names are case-insensitive; the spelling shown matches the observed CLI capture where practical.

| Header | Source and target value | Current gap and required change |
| --- | --- | --- |
| `Authorization` | `Bearer ` + OAuth `access_token` from the selected auth metadata | Add OAuth-specific precedence so an inbound API key or `api_key` attribute cannot override the OAuth token. |
| `Chatgpt-Account-Id` | Original OAuth `account_id` | Keep the current metadata source, but make it mandatory for the OAuth profile and never derive or hash this header value. |
| `Originator` | `codex-tui` | Force this value for OAuth fidelity. Do not accept an inbound or auth custom value on this path. |
| `User-Agent` | Windows: `codex-tui/0.154.0 (Windows 10.0.19044; x86_64) unknown (codex-tui; 0.154.0)`; Mac: `codex-tui/0.154.0 (Mac OS 26.5.2; arm64) unknown (codex-tui; 0.154.0)` | Replace the current Mac/iTerm fallback. Force the selected system profile after custom-header and model-override processing so no later step changes it. |
| `version` | `0.154.0` | Generate the CLI version when absent or inconsistent. The OAuth fidelity profile owns this value. |
| `x-codex-beta-features` | `remote_compaction_v2` | Supply the observed OAuth HTTP default instead of relying on the new-api/client copy. |
| `x-openai-internal-codex-responses-lite` | `true` | Set for OAuth `/responses` requests. Do not depend on a body metadata marker; the observed HTTP CLI request carries the marker in the header. |
| `x-codex-routing-hint` | `model=` + normalized base model, for example `model=gpt-5.6-terra` | Rebuild from the final model after suffix parsing. An inbound routing hint cannot describe a different model. |
| `x-codex-window-id` | Canonical window ID, normally `<session-id>:0` | Rebuild from body metadata or the canonical session fallback and keep it equal to `client_metadata.x-codex-window-id`. |
| `x-codex-turn-metadata` | Canonical JSON string described in Section 7 | Rebuild from the normalized body metadata. Preserve unknown fields, while replacing identity/system fields required by this plan. |
| `x-client-request-id` | Canonical session ID, matching the observed CLI relationship | Rebuild from the same canonical session ID. Do not generate an unrelated request ID when the body already has a session. |
| `session-id` | `prompt_cache_key` when present, otherwise canonical `client_metadata.session_id`, otherwise a generated UUID | Keep the existing prompt-cache behavior but make its selected value explicit and write it back into body metadata. |
| `thread-id` | Canonical `client_metadata.thread_id`; fallback to the session ID | Ensure it is represented in both body metadata and the turn metadata JSON. |
| `Content-Type` | `application/json` | Keep the current behavior. |
| `Accept` | `text/event-stream` for streaming; `application/json` for non-streaming | Keep the current stream-dependent behavior. |
| `Content-Encoding` | `zstd` when the OAuth fidelity encoder is enabled | new-api removes the inbound encoding after decoding. Re-encode the final JSON body in CLIProxyAPI so the ChatGPT wire request matches the observed CLI request. Never claim `zstd` unless the body is actually zstd encoded. |
| `Cookie` | Cookie jar output for the ChatGPT request URL | Stop accepting arbitrary downstream cookie text for OAuth fidelity. Let the account jar supply the cookie header. |
| `Connection` | Transport-managed; retain the current keep-alive behavior unless the wire-fidelity tests show a conflict | This is not an identity field and should not override HTTP/2 transport requirements. |
| `Host`, `Content-Length`, HTTP version | Managed by `net/http` and the custom HTTP/2 round tripper | Do not copy these from new-api. The URL and transport own them. |

The OAuth fidelity builder should run after `util.ApplyCustomHeadersFromAttrs` and `applyModelHeaderOverrides`, then force the protected fields in a final pass. `DisableCodexCloaking` must not disable the requested OAuth identity profile; that flag can continue to govern the existing non-fidelity/API-key behavior.

The observed CLI request does not contain a standalone HTTP `x-codex-installation-id` header. The installation value is carried in `client_metadata.x-codex-installation-id` and repeated as `installation_id` inside the JSON turn metadata. The implementation must update those body locations; it must not invent a separate HTTP header unless a later capture proves that one is required.

### 6.2 Header precedence

For the OAuth fidelity path, the precedence is:

```text
OAuth access_token / account_id
  > canonical values derived from the final body
  > fixed Codex CLI constants
  > inbound client values
  > auth custom headers
```

The inbound values remain useful as input for session/thread/window continuity, but they cannot replace OAuth identity, system UA, version, beta features, originator, Lite, or routing hint.

## 7. Body and metadata normalization

### 7.1 Existing Codex conversion that remains in force

The current translator and executor already perform compatibility work. The fidelity change should preserve these behaviors and make them visible in tests:

- A string `input` becomes a user message array.
- `stream` is set to `true` for the streaming path.
- `store` is set to `false`.
- `include` is normalized to `["reasoning.encrypted_content"]`.
- `parallel_tool_calls` is set to `false` for a Responses Lite request. The generated Lite header must be visible to this normalization step; otherwise the current helper may take the non-Lite branch.
- Unsupported token/decoding fields are removed: `max_output_tokens`, `max_completion_tokens`, `temperature`, `top_p`, non-`priority` `service_tier`, `truncation`, `prompt_cache_options`, and `prompt_cache_retention`.
- Nested `prompt_cache_breakpoint` hints are removed.
- `context_management` is removed for Codex compatibility.
- The unsupported top-level `user` field is removed.
- `system` roles in the input array become `developer` roles.
- Known legacy built-in tool aliases are normalized.
- The executor removes `previous_response_id`, `generate`, and `prompt_cache_retention`. Before the final OAuth/API request is sent, `safety_identifier` is always replaced with the `chatgpt_user_id` claim decoded from the selected OAuth access token (or the persisted value for legacy auth files). In the stream path it preserves only the supported `stream_options.reasoning_summary_delivery` subfield; the non-stream path removes `stream_options`.
- Non-native compatibility requests may gain an empty top-level `instructions`; native Codex requests keep the absence of `instructions`.
- Existing image-tool, reasoning-encrypted-content, tool-schema, multi-agent, replay-cache, and response translation logic remains in its current stage.

These are protocol transforms, not arbitrary prompt injection. No new fixed assistant instruction or user-visible prompt should be inserted by the OAuth fidelity change.

### 7.2 Canonical metadata model

Parse these locations when present:

```text
client_metadata.x-codex-installation-id
client_metadata.x-codex-window-id
client_metadata.session_id
client_metadata.thread_id
client_metadata.turn_id
client_metadata.root_turn_id
client_metadata.x-codex-turn-metadata   (JSON string)
prompt_cache_key
```

The nested turn metadata JSON is parsed as an object. Unknown keys are preserved. The known fields are rewritten as follows:

| Field | Target rule |
| --- | --- |
| `installation_id` | Derived account installation ID from Section 8. |
| `session_id` | Canonical session ID. |
| `thread_id` | Canonical thread ID. |
| `turn_id` | Preserve the incoming turn ID; generate a UUID only when the request has no usable turn ID. |
| `window_id` | Canonical window ID. |
| `sandbox` | Preserve the incoming value for system detection and metadata continuity. Missing/malformed values select Mac and remain absent unless they already existed. |
| `timezone` fields, if present as structured metadata | Normalize to `Asia/Singapore`. |

The outer `client_metadata` receives the same canonical values. In particular, both `client_metadata.x-codex-installation-id` and the nested `installation_id` must match exactly. If an outer or nested turn metadata value is malformed JSON, retain unknown raw content where possible, use the safe Mac default for system selection, and populate headers from the valid outer/body identifiers. Do not silently use an API key as a fallback identity.

### 7.3 Identifier resolution

Use one resolver so the body and headers cannot drift:

```text
sessionID = prompt_cache_key
         or client_metadata.session_id
         or nested turn metadata.session_id
         or generated UUID

threadID  = client_metadata.thread_id
         or nested turn metadata.thread_id
         or sessionID

turnID    = client_metadata.turn_id
         or nested turn metadata.turn_id
         or generated UUID

windowID  = client_metadata.x-codex-window-id
         or nested turn metadata.window_id
         or sessionID + ":0"

clientRequestID = sessionID
```

When `prompt_cache_key` exists, it remains the session affinity key used by the current `cacheHelper`. The resolver writes the selected value back to `prompt_cache_key` only when the existing cache logic would otherwise generate a different session key.

## 8. Installation identity derivation

The requested identity is tied to the OAuth account, not to the caller or the server process.

### 8.1 Recommended wire format

Use a valid UUID on the wire so ChatGPT continues to receive the protocol shape used by the CLI:

```text
installationUUID = UUID-format(MD5(accountID + ":" + system))
```

`accountID` is the exact OAuth account ID string. `system` is the lower-case value `windows` or `mac`. The MD5 digest is formatted using the standard UUID grouping (`8-4-4-4-12`). This gives each account a stable but system-specific installation identity while retaining a strict UUID format.

The following locations must receive exactly `installationUUID`:

```text
client_metadata.x-codex-installation-id
client_metadata.x-codex-turn-metadata.installation_id
any outgoing x-codex-turn-metadata installation_id
```

The `Chatgpt-Account-Id` header remains the original account ID. It is not replaced with the hash or UUID.

### 8.2 Literal suffix interpretation

Appending `-windows` or `-mac` after a UUID would produce a non-UUID value. The recommended implementation therefore encodes the system in the MD5 input and keeps the wire value valid. If a later compatibility capture proves that ChatGPT accepts and requires a literal suffix, that must be added as a separately documented protocol decision with a dedicated test; it should not be introduced silently.

### 8.3 Interaction with `identity-confuse`

The existing `identity-confuse` feature derives a different installation value from `auth.ID` and the incoming installation value. It must not overwrite the account-derived OAuth fidelity value. The implementation should either:

1. bypass installation rewriting in `applyCodexIdentityConfuseBody` when the OAuth fidelity profile is active; or
2. run the account-derived rewrite after identity-confuse and update every body/header copy again.

Option 1 is preferred because it avoids two competing identity policies. Existing prompt-cache/turn-ID obfuscation must also be reviewed so it cannot make the header/body resolver disagree. The default configuration currently has `identity-confuse: false`, but the precedence must remain deterministic when an operator enables it.

## 9. System detection and User-Agent

The detector reads `sandbox` from the nested turn metadata first, then the equivalent body metadata if available:

```text
if sandbox == "windows_elevated":
    system = windows
    user-agent = codex-tui/0.154.0 (Windows 10.0.19044; x86_64) unknown (codex-tui; 0.154.0)
else:
    system = mac
    user-agent = codex-tui/0.154.0 (Mac OS 26.5.2; arm64) unknown (codex-tui; 0.154.0)
```

The inbound User-Agent is not consulted. It may belong to new-api, an SDK, or a browser and is intentionally overwritten. A missing or malformed `sandbox` selects Mac exactly as requested.

The system choice is made once per request and passed to both installation-ID derivation and header construction. It must not be inferred a second time from the final UA.

## 10. Timezone and prompt text replacement

The replacement is deliberately scoped to environment metadata embedded in text content. For each text part in the Codex input/environment block:

```xml
<timezone>any existing value</timezone>
```

becomes:

```xml
<timezone>Asia/Singapore</timezone>
```

Rules:

- Preserve the surrounding XML, indentation, and all unrelated text.
- Apply the same value to a structured timezone field if one exists in turn metadata.
- Do not perform a global byte replacement of the account ID, installation UUID, or the word `timezone` across arbitrary user messages.
- Do not modify encrypted reasoning content.
- Do not modify tool arguments, file contents, URLs, or ordinary user text that merely mentions a timezone.
- If the environment block has no timezone tag, do not inject a new user-visible block solely for this feature.

If a future capture establishes that the CLI emits additional fixed environment fields, add those fields as explicit, tested transformations rather than broad prompt replacement.

## 11. Cookie jar design

### 11.1 Storage model

Create one in-process `http.CookieJar` per OAuth `auth.ID`. A registry can be held by the Codex executor or a dedicated helper in `internal/runtime/executor/helps/`:

```text
cookieJarRegistry[auth.ID] -> *cookiejar.Jar
```

The registry must be concurrency-safe. A jar is never shared between two auth IDs, even when they have the same email, account ID, proxy, or token history.

Use `net/http/cookiejar` (or an equivalent implementation) and attach the jar to the `*http.Client` returned by `helps.NewUtlsHTTPClient`. The custom uTLS/HTTP2 transport remains unchanged; `http.Client` performs cookie selection before the round trip and stores `Set-Cookie` after the response arrives.

### 11.2 Request/response behavior

For a ChatGPT request:

1. Resolve the jar by the selected OAuth `auth.ID`.
2. Set `client.Jar = jar` before `Do`.
3. Do not copy a raw downstream `Cookie` header into the request. The jar decides which cookies match the URL, domain, path, Secure flag, and expiry.
4. Allow `http.Client` to process all eligible `Set-Cookie` values from the response.
5. Keep the jar available for the next request using that auth ID.

Only cookies applicable to the ChatGPT host should be retained for this feature. Cookies for unrelated hosts must not be imported from the downstream request and must not be forwarded to another provider.

### 11.3 Lifecycle

- New OAuth auth: create an empty jar lazily on the first ChatGPT request.
- Token refresh: retain the jar because the account identity is unchanged.
- Auth removal, credential replacement, logout, or explicit invalidation: delete the jar for that `auth.ID`.
- Auth reload with a new runtime identity: do not reuse the old jar solely because the email or account ID looks similar; use the new auth record identity and an explicit migration only if later required.
- Process restart: default to an in-memory jar. Persisting cookies to disk is out of scope for this plan; if added later, use encrypted, permission-restricted storage and a versioned format.

### 11.4 Logging and downstream response rules

- Never log the `Cookie` header value.
- Never log `Set-Cookie` values.
- Existing header masking must continue to redact `Authorization`, `Cookie`, and `Set-Cookie`; add a regression test if the new jar path introduces a new logging surface.
- Do not include `Set-Cookie` in `cliproxyexecutor.Response.Headers` returned to the downstream API client.
- Strip upstream-only cookie and connection headers at the API response boundary. The jar is an internal OAuth session facility.

## 12. Compression and transport

new-api decodes an incoming zstd body before forwarding it, and removes the incoming `Content-Encoding` header. The OAuth fidelity path enables outgoing zstd by default and should therefore:

1. Normalize the JSON body first.
2. Encode the final bytes with the existing `github.com/klauspost/compress/zstd` dependency.
3. Set `Content-Encoding: zstd` only when encoding succeeds.
4. Set the request body and length to the encoded bytes.
5. Fail the request rather than sending a falsely labeled body if encoding fails.

A future compatibility setting may disable compression, but the default OAuth fidelity behavior is to send the `zstd` encoding shown in the CLI capture.

The response remains streamed through the existing Codex SSE scanner and translator. HTTP/2, TLS fingerprinting, proxy selection, and context cancellation continue to be supplied by `NewUtlsHTTPClient`. Do not add an executor timeout after the upstream connection is established.

## 13. Response handling

The existing response pipeline remains the source of client-visible behavior:

- For HTTP errors, read and classify the upstream body, apply any enabled identity response restoration, and return the existing Codex status error.
- For streaming success, scan SSE lines, preserve Codex metadata events, restore any internal identity mapping, apply multi-agent response restoration where enabled, and translate to the requested downstream format.
- For non-stream success, translate the completed Codex response and preserve usage normalization.

The only response-specific addition in this plan is cookie handling: `Set-Cookie` is consumed by the account jar, then removed from downstream response headers. The ChatGPT response body and normal protocol headers continue through the existing translation path.

## 14. Proposed code organization

Keep executor files focused and put reusable support code under `internal/runtime/executor/helps/`, in line with the repository conventions.

Suggested concepts and responsibilities:

```text
helps/codex_oauth_identity.go
  - OAuth access-token/account-id accessors
  - sandbox system detection
  - account installation UUID derivation
  - canonical session/thread/turn/window resolver

helps/codex_oauth_metadata.go
  - client_metadata parsing and reconstruction
  - nested x-codex-turn-metadata parsing
  - scoped environment timezone replacement

helps/codex_cookie_jar.go
  - per-auth jar registry
  - concurrency-safe get/delete/invalidate operations

codex_executor_request.go
  - final OAuth header builder
  - final body/header consistency pass

codex_executor_execute.go
  - invoke normalization before cacheHelper
  - attach the per-auth jar to the uTLS client
  - preserve existing stream/non-stream response handling
```

The exact file split can be adjusted during implementation, but the following functions should have direct unit coverage:

```text
buildCodexOAuthRequestHeaders
rewriteCodexOAuthPayload
detectCodexSystem
deriveCodexInstallationUUID
resolveCodexRequestIdentity
replaceCodexEnvironmentTimezone
codexCookieJarForAuth
```

## 15. Interaction with existing configuration

The default OAuth fidelity behavior should not depend on account plan metadata or a plan-specific branch.

Recommended policy:

- OAuth HTTP `/responses`: fidelity profile enabled by default.
- API-key Codex HTTP requests: preserve the current API-key behavior.
- `CodexHeaderDefaults`: retain for existing compatibility/WebSocket/API-key fallback behavior, but do not let it override the fixed OAuth fidelity UA, version, beta, or originator.
- `disable-codex-cloaking`: do not let it undo the explicitly requested OAuth fidelity headers. If this creates a compatibility concern, introduce a separately named setting rather than overloading the meaning of the existing flag.
- `identity-confuse`: installation identity from this plan has priority for OAuth fidelity; prompt-cache and turn-ID obfuscation must be reconciled with the canonical metadata resolver.
- `stream-bootstrap-buffering`, multi-agent optimization, image-generation injection, reasoning replay, and model-level cooling retain their existing semantics.

## 16. Test plan

Tests should assert observable requests and responses with deterministic fixtures. They should be consolidated in the existing Codex executor test areas rather than spread across unrelated packages.

### Header and identity tests

- `sandbox: windows_elevated` produces the exact Windows UA.
- Any other sandbox value, missing sandbox, or malformed turn metadata produces the exact Mac UA.
- OAuth `Authorization` uses `access_token` even if the inbound request has `Authorization: Bearer client-api-key` or auth attributes contain an API key.
- `Chatgpt-Account-Id` equals the OAuth metadata account ID exactly.
- The client API key does not occur in outbound headers or the encoded body.
- `Originator`, `version`, beta features, Lite, and routing hint are forced to the target values.
- Routing hint follows the normalized base model after model suffix parsing.
- Session, thread, window, client request, prompt cache, and nested turn metadata agree.
- Unknown turn metadata fields survive reconstruction.
- The account-derived installation UUID is deterministic for the same account/system and differs between Windows and Mac.
- The installation UUID is identical in outer metadata, nested metadata, and outgoing turn metadata.
- `identity-confuse` cannot overwrite the OAuth installation identity.

### Body and prompt tests

- Existing unsupported-field removal remains intact.
- Responses Lite normalization sets `parallel_tool_calls: false` when the generated Lite header is present.
- Native Codex input does not gain an `instructions` field.
- Non-native compatibility input retains the existing empty-instructions behavior.
- Only `<timezone>...</timezone>` in the targeted environment text changes to `Asia/Singapore`.
- User messages, tool arguments, URLs, and encrypted reasoning content remain unchanged.
- A malformed nested metadata string does not cause an API-key fallback or an unrelated prompt rewrite.

### Cookie tests

- A fixture response with `Set-Cookie` stores the cookie in the jar.
- The next request for the same auth ID sends the eligible cookie automatically.
- A request for a different auth ID does not send the first account's cookie.
- Domain/path/Secure/expiry rules are honored by the jar.
- Deleting or invalidating an auth removes its jar.
- Refreshing an OAuth token keeps the account jar.
- Request and response logging never exposes cookie values.
- `Set-Cookie` is absent from downstream response headers.

### Wire and regression tests

- An inbound zstd body is represented as plain JSON at the CPA ingress boundary, then emitted as valid zstd when the OAuth fidelity encoder is enabled.
- The upstream fixture can decode the body and sees the normalized JSON, while the captured request header says `Content-Encoding: zstd`.
- Streaming and non-streaming requests both attach the correct jar and use the correct `Accept` header.
- Existing Codex native-fidelity tests continue to pass for response metadata and completion events.
- OAuth refresh, request retry, and concurrent same-account requests do not race on the jar or canonical metadata.

## 17. Acceptance criteria

The implementation is ready for review when a test fixture can capture one complete CPA-to-ChatGPT request and demonstrate all of the following at once:

1. URL is `/backend-api/codex/responses`.
2. OAuth token and original account ID are used.
3. The fixed CLI version, beta, Lite, originator, routing hint, and system-specific UA are present.
4. Session/thread/window/request IDs match the normalized body.
5. The installation UUID is account-derived, system-specific, and repeated consistently in body/header metadata.
6. The environment timezone is exactly `Asia/Singapore`.
7. The body is valid JSON after decompression and valid zstd on the wire when enabled.
8. The account jar stores and reuses ChatGPT cookies without cross-account leakage.
9. The downstream response contains the existing translated Codex result and no upstream cookie values.

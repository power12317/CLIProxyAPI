# CPA / Codex inference bridge v1

Transport: app-server WebSocket over a Unix-domain socket. One inference per connection. Normal app-server initialize (clientInfo, capabilities.experimentalApi=true), then initialized, precedes CPA calls. Notifications use the app-server envelope without a jsonrpc field. IDs for calls are integers; requestId is an opaque string.

## Capabilities

`cpa/capabilities/read`, params `{}`:

```json
{"protocolVersion":1,"runtimeVersion":"fork-version","upstreamRevision":"commit","credentialId":"worker-a","accountId":"workspace-id","authMode":"chatgpt","executionMode":"inference-only","rawEvents":true,"operations":["responses"],"persistentSessions":false}
```

accountId/authMode may be null while signed out. CPA must verify the protocol, credential ID, expected account, managed auth mode, execution mode, and requested operation before submitting. The worker must recheck account identity when submitting, including concurrent logout/account changes. Capability results never contain tokens.

## Inference

`cpa/inference/start`:

```json
{"requestId":"opaque-id","credentialId":"worker-a","accountId":"workspace-id","operation":"responses","sourceFormat":"openai-response","sessionId":"caller-and-account-scoped-id","request":{"model":"model-id","input":[],"stream":true}}
```

request carries Responses semantics, not an agent prompt. The worker validates supported fields and uses its own identity/auth metadata; it must not silently replace caller instructions/tools/tool_choice. CPA forwards no Authorization or Cookie header. persistentSessions=false requires rejection of previous_response_id and prewarm/generate=false. Compaction requires operations to include responses/compact.

After upstream headers, reply to start with `{requestId,statusCode,headers}` where headers is a map of arrays of strings. This reply MUST precede inference notifications. Non-success before acceptance uses JSON-RPC error `{code,message,data:{httpStatus}}`. CPA does not retry an ambiguous accepted request.

Ordered notifications:
- `cpa/inference/event`: `{requestId,event:<complete upstream Responses JSON event>}`.
- For compact: `cpa/inference/result`: `{requestId,body:<complete compact JSON>}`.
- `cpa/inference/error`: `{requestId,httpStatus,message}` for an accepted request that fails.
- `cpa/inference/completed`: `{requestId}`, exactly once after events/results/errors.

A Responses request needs a valid response.completed/response.incomplete/response.failed terminal event, a raw upstream error event, or an explicit inference error; completed alone is not a model completion. Preserve wire fields and event order alongside parsed internal events without bypassing required safety buffering. Unknown events retain their raw JSON.

`cpa/inference/cancel`, params `{requestId}`, result `{}`. Connection closure also cancels only that connection's inference. Runtime auth/background workers remain alive. Bounded queues propagate backpressure. No network read/idle/total inference timeout after connection establishment.

## Security and upgrades

One persistent managed identity per worker. Private socket mount; do not expose a public unauthenticated listener. Never echo tokens, cookies, or request bodies in errors/logs. Strip upstream secret and hop-by-hop response headers. CPA namespaces sessions by caller, credential, and supplied conversation key. Unsupported operations fail closed; no silent downgrade to a legacy executor. Release metadata must identify upstream and fork revisions plus supported protocol major/capabilities. Real-account tests are separate from deterministic contract tests.

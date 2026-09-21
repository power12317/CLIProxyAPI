# Codex turn-state tickets

简体中文版本：[codex-turn-state-tickets_CN.md](codex-turn-state-tickets_CN.md)

CLIProxyAPI can proactively obtain the one-hour `x-codex-turn-state` value used by
ChatGPT Codex OAuth accounts. The feature is disabled by default. When enabled,
the harvester probes each active Codex OAuth credential and configured model.
When `codex.turn-state-ticket.harvest-proxy-url` is configured it is used for
the probe; when empty, the probe uses the direct transport. Normal business
requests continue to use the credential's own proxy.

```yaml
codex:
  turn-state-ticket:
    enabled: true
    harvest-proxy-url: "socks5://user:password@residential.example:1080"
    probe-interval-seconds: 60
    models: [gpt-6-astra, gpt-5.6-sol]
    fail-closed: true
```

Each accepted response must be HTTP 200 and contain a Fernet-looking value with
the account-specific length. Personal accounts (`free`, `plus`, and `pro`) use
292; Team and Business accounts use 332. The value is stored per auth ID and
model in the auth metadata, expires after one hour, and is refreshed ten minutes
before expiry. The state blob is never returned by management APIs or auth-file
downloads; management responses expose only readiness, length, and remaining
seconds. A failed probe leaves the previous valid ticket untouched.

The background probe interval defaults to 60 seconds (one minute) when omitted
or non-positive. Explicit positive intervals are preserved; change an existing
`probe-interval-seconds: 6` to `60` to use the one-minute interval.

Startup with harvesting enabled and a disabled-to-enabled config change both
start a sweep immediately. Fresh tickets outside the refresh window are skipped.
All probes run sequentially in credential-ID and configured-model order, including
replacement probes requested by normal traffic. The next periodic sweep starts
one configured interval after the previous sweep finishes.

A probe response with a 312-byte turn-state pauses the rest of that account's
probes for the current sweep and for at least one configured interval after the
response. Credentials sharing an email or account ID share this pause, including
linked credentials with only one of those fields; matching trims whitespace and
ignores case. Credentials without either field fall back to their own credential
ID. Other accounts continue. Reloads, re-enabling, and replacement requests cannot
bypass an active pause. Ticket storage remains separate per credential and model.

When `fail-closed` is true, a configured model is not sent upstream without a
valid ticket. The request returns a retryable executor error so the auth manager
can try another credential. When it is false, the request proceeds using the
existing turn-id-scoped passive cache until a target-length ticket is observed.
A valid account-specific ticket (292 for Personal, 332 for Team/Business) still
always replaces the request's turn-state value, and then remains authoritative
independently of `turn_id`.

Normal responses with the account's target length are stored immediately as the
account/model ticket and reused. Other response lengths, including 312, are not
promoted to the proactive ticket; the current turn continues to use the existing
passive cache. A Personal 312 response invalidates an active 292 ticket and
queues a fresh probe on the sequential worker, subject to an active account pause.

If ChatGPT returns a 312-byte `x-codex-turn-state` while a valid Personal 292 ticket is
active, CPA immediately invalidates and removes that account/model ticket and
queues a fresh harvest probe, subject to an active account pause. The next request therefore waits for the new
ticket when fail-closed is enabled.

The management API exposes `GET /v0/management/codex-turn-state-ticket` and
`PUT`/`PATCH` on the same path. The proxy URL is masked in responses; sending
the masked value keeps the stored password unchanged. The API also returns
redacted per-account ticket readiness. The `/v0/management/auth-files` entries
include the same status under `codex_turn_tickets` when the feature is enabled.

HTTP, SSE, and WebSocket Codex transports all apply the ticket after ordinary
header construction. WebSocket request bodies mirror it into
`client_metadata.x-codex-turn-state` because a reused socket has no new HTTP
handshake. The existing response-observed turn-state cache is retained for
models outside the proactive policy. For a model inside the proactive policy,
the account/model ticket is authoritative and is independent of `turn_id`.

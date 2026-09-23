# Codex turn-state retention

简体中文版本：[codex-turn-state-tickets_CN.md](codex-turn-state-tickets_CN.md)

CLIProxyAPI retains the latest 780-byte `x-codex-turn-state` returned for a Codex
OAuth credential and model. All account plans use the same length. The master
switch is disabled by default; normal-response capture for all models defaults
to enabled when the master switch is on.

```yaml
codex:
  turn-state-ticket:
    enabled: true
    cache-all-models: true
    ttl-seconds: 3600
    refresh-before-seconds: 600
    harvest-proxy-url: ""
    probe-interval-seconds: 60
    attempt-timeout-seconds: 25
    fail-closed: true
    models: [gpt-6-astra, gpt-5.6-sol]
```

## Retention and replacement

A normal HTTP response or WebSocket response metadata event containing a 780-byte
state immediately replaces the saved credential/model ticket and restarts the
configured local retention period, one hour by default. Ticket contents are opaque;
the old `gAAAAA` prefix is not required. Normal response capture does not wait for
model generation to complete. Sending a saved ticket alone does not extend its expiry.

A missing header or any other length leaves the saved ticket and expiry unchanged.
Response lengths no longer invalidate tickets, wake replacement probes, or pause
other credentials sharing an account ID or email. The existing HTTP error handling
remains independent of ticket retention.

Stored tickets override the request state across turn IDs, separately for each
credential and model. Mac and Windows credentials never share tickets. Persistence
merges only the changed model into the latest credential, preserving sibling tickets
and concurrent credential edits.

## Startup and timed renewal

Startup with the master switch enabled, and a disabled-to-enabled config change,
both start one immediate sweep. Fresh, unexpired 780-byte tickets outside the renewal
window are skipped. Reloads that keep the switch enabled do not start extra sweeps.

Only configured `models` are probed. All probes run sequentially in credential ID
and configured model order. Probes read the HTTP response header and accept a 780-byte
state from HTTP 200. They use the optional `harvest-proxy-url`, or direct transport
when empty. Normal traffic retains its existing credential proxy. The probe input
remains `ping`, with the response instruction `Reply with exactly: pong`; device
convergence continues to share the normal credential's installation ID.

Timed sweeps remain enabled after startup. The next check starts one configured
interval after the preceding sweep finishes, 60 seconds by default. Configured
models with no usable ticket or within the renewal window are probed. The renewal
window defaults to ten minutes before the most recently saved expiry. A normal
response replacement therefore postpones renewal for that model. For example,
a ticket received at 10:00 expires at 11:00; a replacement received at 10:20 moves
expiry to 11:20 and the renewal window to 11:10.

A probe that returns no usable replacement preserves the old ticket until expiry.
Due or missing configured tickets can be tried at the next scheduled check; normal
response lengths never trigger an extra check. A 312-byte probe response no longer
skips the remaining models or related credentials in the sweep.

With `cache-all-models: true`, Luna, Terra, and other unlisted models capture and
reuse normal-response tickets without adding any proactive probes, including in
the renewal window or after expiry. Their tickets stop overriding requests at expiry
and can be repopulated by a later normal response. With the sub-switch false, only
listed models participate; already stored extra tickets are retained but excluded
from injection and status reporting. Turning off the master switch stops capture,
injection, and proactive probing.

## Missing tickets and compatibility

`fail-closed` keeps its existing meaning for configured models: true returns a
retryable missing-ticket executor error, allowing the auth manager to try another
credential; false lets the normal request proceed with its existing turn-scoped
passive cache. Unlisted models are never blocked for missing tickets. Omission of
`fail-closed` yields false; the example above explicitly sets it to true.

Old YAML `target-length` values remain readable but the effective target is always
780. Saved 292/332 tickets are no longer injected as retained tickets. Startup can
populate configured models, while unlisted models wait for normal responses. OAuth
credentials and other metadata are preserved.

## Management and logs

`GET /v0/management/codex-turn-state-ticket` reports the effective policy and redacted
credential/model readiness. `target_length`, `personal_target_length`, and
`team_target_length` all return 780. PUT/PATCH support the existing settings; an
explicit `target_length` must be 780. The all-model field is `cache_all_models`;
omitting it on an update preserves the current value.

Ticket status lists configured models first, then already stored extra models by
name when all-model capture is enabled. The auth-files endpoint returns the same
summary under `codex_turn_tickets`. Raw tickets are excluded from management responses
and auth-file downloads; proxy passwords remain masked.

HTTP, SSE, compact, and WebSocket requests apply retained tickets after ordinary
header construction. WebSocket frames also receive the saved state in
`client_metadata.x-codex-turn-state`, including on reused connections.

Access logs and request monitoring retain the actual final request/response lengths:
`0/780` captures an initial ticket, `780/0` keeps the current ticket and expiry, and
`780/780` saves the response ticket and restarts retention. Other response lengths
leave a still-unexpired saved ticket intact. The probe log format stays unchanged:

```text
[2026-09-23 10:00:00] [a1b2c3d6] [codex-user-team-windows.json] [7a120003] [8b230003] [info ] [TICKET-PROBE] 200 | 1.420s | gpt-5.6-sol/- | 0/780 | POST "/backend-api/codex/responses"
```

Only lengths are logged. `780/780` is not an invalidation or failure classification.

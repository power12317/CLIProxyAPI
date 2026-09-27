# Plan B v2: shared CPA credentials and panel control

The approved v2 design replaces the initial Unix/native-auth prototype. CPA uses
branch codex/plan-b-managed-auth; Codex uses codex/cpa-managed-auth; CPAMP uses
codex/codex-runtime-control.

CPAMP controls the master switch, worker configuration, and per-credential
preference. CPA routes inference over a dedicated authenticated WebSocket on
loopback port 38317 onward. The worker directly reads/writes one configured CPA
credential JSON; no separate refreshed auth.json copy exists. codex_cli.enabled
is the preference, worker_id selects the worker, and owner is the effective
cpa/codex owner. Turning the master off preserves preferences and returns files
to CPA. Both sides reload the common file rather than using a stale token copy.

OAuth is official browser authorization: CPAMP requests a URL through CPA, then
submits the user's complete callback URL through CPA to Codex. Codex performs
the official PKCE/state/code exchange and saves into the same credential file.
Pending login survives separate short-lived management WebSocket connections.

The user explicitly requested the simplest implementation and declined new
cross-process locks, leases, epochs, CAS and handoff protocols. None are added.
Concurrent refresh and switching are not guaranteed to be mutually exclusive.
Any additional protective mechanism requires separate subsequent user approval.
No request-count UI, background health polling, or Docker controller is added.

Inference keeps the existing full Responses event path without tool execution or
automatic continuation. Ordinary runtime lifecycle stays enabled, while credential
activity follows the selected owner. Disabled CPA does not contact the worker
and remains usable without a running worker. File-backed storage is the supported
deployment; remote credential stores and Home are outside this implementation.

Contract: PROTOCOL_V2.md. Deployment: README_CN.md. Plan A remains deferred.

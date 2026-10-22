# Shared model-only RPC extension contract

Interface scope: main and codex/native-model-runtime, after their
completed caller-input compatibility fixes. No upstream-main merge or executor
unification is required. This contract contains no private protocol number,
contractId, hash, or replacement branch-version gate.

## Endpoint and initialization

- Connect to ws://<host>:38317/ (root path). Health routes remain unchanged.
- Keep the official initialize / initialized JSON-RPC message shapes, including
  official clientInfo and capability behavior. clientInfo.version is the actual
  caller product version; Codex Server child initialization remains 0.162.1.
- The cpa/* model-only, credential, OAuth, diagnostics and ticket messages below
  are explicit fork extensions carried by the official app-server envelope.
  They are not official thread/start or turn/start and never execute client tools.

## Capability response

Use cpa/capabilities/read after initialization. No private numeric protocol version is exchanged.
Keep existing runtimeVersion, upstreamRevision, executionMode, rawBody,
responseIdentityMapping, manualOAuth, upstreamLogs, upstreamBodyLogs and operations.
Product/source metadata are informational and must not become protocol-number gates.

Only four small factual fields are added; ticketSnapshots is reused:

| Field | main | native |
| --- | --- | --- |
| executionMode (existing) | inference-only | inference-only |
| rawBody (existing) | true | true |
| businessNotification | cpa/inference/body | cpa/inference/event |
| completionSemantics | http-body-eof | native-terminal |
| diagnosticsCompleted | false | true |
| responsesWebsocket | false | true |
| ticketSnapshots (existing on main) | true | false |

These describe actual behavior; they are not a new execution switch or opaque
contract identifier. responsesWebsocket means the Responses implementation has a
native WS path, not that every model/provider/configuration guarantees WS. HTTP-only
requests cannot claim prewarming. Keep specific per-request unsupported errors.
Missing optional ticketSnapshots means false. Do not require all capability fields
to equal one fixed template. Reject only a genuinely missing required behavior or
malformed essential field, and explain that behavior rather than a version mismatch.

## Request, business bytes and completion

- cpa/inference/start/cancel and existing parameter/result shapes remain intact.
  Keep requestId, credentialId, operation, sourceFormat, sessionId, imageApi and
  the normalized business request; optional envelope additions remain tolerant.
- The main-only captureTicketSnapshot feature is used only with ticketSnapshots.
  A required snapshot on a worker that advertises false is unsupported; callers
  must not pretend that a model success produced a ticket.
- Register/accept both existing business notification names. Route the byte stream
  using businessNotification. Each carries requestId and bodyBase64; decode once.
- Main bytes retain raw HTTP business data; completed means HTTP EOF. CPA must
  interpret response/image events to determine model-level success.
- Native bytes are accepted native-parser business output; completed means native
  model/image success. The completion notification remains payload-free.
- Preserve real-time first event, continued deltas/partials and cancellation.
  Errors after acceptance use cpa/inference/error and never successful completion.
  No local Agent/tool loop, inference replay, or CPA direct-upstream fallback is added.

## Diagnostics and tickets

- Keep existing cpa/inference/upstream payloads and original raw IPC logging.
  Optional/unknown diagnostic details do not establish model success or fail it.
- When diagnosticsCompleted is true, keep the independent diagnostic route alive
  until cpa/inference/diagnostics/completed, in either order with business terminal.
  When false, use main's existing lifecycle; do not wait for a nonexistent terminal.
- Keep main cpa/inference/ticket payloads, privacy, opt-in, exchange provenance,
  ordering and captured cookie expiry behavior. Native ticketSnapshots=false is
  honest absence, not a reason to remove main's existing feature.
- Retries belong to CPA/the client. Both worker model send paths remain zero-retry,
  including reqwest protocol retries and post-401 authentication resubmission.

## Deployment and verification

Update root URLs consistently in repository scripts, examples, deployment docs,
fixtures and tests. Keep port, health checks, credentials, product version,
User-Agent, originator, binary names, image names, compose names and release triggers.
Regenerate JSON/TS/Python/precomputed artifacts with the repository script.

Verify both root-path handshakes and existing model-only flows: full tool/input/
output schemas and explicit controls; body/event choice; EOF/native completion;
independent native diagnostics; main ticket capture; image JSON/multipart/stream;
actual first/delta delivery; cancellation and zero model retries. CPA owns its
matching changes in its own repository; Codex Server modifies no CPA/CPAMP source.

Implementation artifacts and final immutable binaries will be recorded alongside
this file. Deploy matching CPA and Codex Server images when upgrading from the removed endpoint.

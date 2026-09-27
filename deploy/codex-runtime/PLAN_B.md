# Plan B v3: one Codex master with independent account processes

This is the current approved Plan B. It supersedes the v1 Unix-socket prototype
and v2 container-per-account deployment. CPA uses `codex/plan-b-managed-auth`,
Codex uses `codex/cpa-managed-auth`, and CPAMP uses `codex/codex-runtime-control`.
Plan A remains deferred on its separate reserved branch.

## Responsibilities

CPA retains client-facing protocols, credential selection, model registration,
request/response processing, logs, and existing accounting. Its existing file-backed
`Auth.ID` is the bridge `credentialId`. There is no separate worker identifier,
account binding parameter, or replacement credential filename.
CPA's existing credential discovery rules are unchanged; a relative-path bridge
identity does not add recursive credential loading to CPA.

A single Codex master container shares CPA's network namespace and original auth
directory. It monitors the files, starts one account process for each enabled
credential, and routes CPA requests by that same credential ID. Each account
process has a private persistent `CODEX_HOME`; only its original CPA credential
file is shared. Independent processes and data directories are the intended
isolation boundary, not separate containers.

The master listens on `127.0.0.1:38317` with no published port or bridge key.
The deployment needs only the shared auth directory and one persistent Codex data
volume. The master supplies the original credential path to its child process
internally; Compose does not configure per-account paths or IDs.

## State and lifecycle

CPA saves the global switch and per-credential preferences in its configuration.
Preferences are keyed by the original `Auth.ID`; disabling the global switch does
not discard them. The only effective runtime field in each shared credential is
`codex_cli.enabled`. The master starts an account process only when that field is
true and the credential's top-level `disabled` field is not true.

After saving effective file state, CPA sends a credential reload notification over
the existing WebSocket control channel. The master applies that same state when
monitoring files and at startup. Closing a mode stops the corresponding account
process and its credential maintenance and background account activity. The master
remains available for local control. Token refresh writes the same original file
and does not recreate the account process merely because token fields changed.

CPA reads the latest original file when returning to native execution. Existing
requests follow cancellation semantics rather than switching upstream executors
halfway through a response. There is no cross-process lock, lease, epoch, CAS,
refresh mutex, or automatic replay. A request already sent upstream cannot be
retracted by changing a local flag.

## Inference and authorization

The account process maps CPA request semantics into the native Codex request
builder and uses native authentication and transport. It does not start an agent
turn or execute returned tool calls. The original HTTP status, headers, and body
bytes return through the master to CPA for ordinary response processing.

Actual upstream requests, response metadata, failed bodies, and transport errors
enter CPA's existing request-log pipeline. The raw body is the only business
response channel; it is not duplicated as parsed Codex events. No extra usage
collector, request counter, or monitoring UI is introduced.

CPAMP communicates only with CPA. The selected CPA instance network configuration
contains one small mode switch next to the Basispoints switch. Authorization stays
on the existing OAuth page using `/codex-auth-url`, `/oauth-callback`, and
`/get-auth-status`; CPA selects the implementation internally. There is no separate
runtime menu, standalone runtime page, or account authorization section in settings. CPA delegates login to the
master and forwards the authorization URL and callback result. Reauthorization
uses the original credential file. A newly requested login creates a new CPA
credential identity, which becomes the account process identity.

## Deployment and validation

The supported deployment uses local shared file storage. Home and remote credential
stores are outside this test branch. Ordinary Responses HTTP/SSE is the implemented
bridge operation; protocol capability negotiation declares the available operations.

Use the generic [Compose overlay](compose.override.yaml) with your privately maintained deployment configuration.
The exact frozen wire contract is [PROTOCOL_V3.md](PROTOCOL_V3.md). Earlier protocol
and validation documents are historical and do not establish v3 test results.
Pushes to each development branch trigger its Docker build. Delivery confirms the
push and Actions trigger without waiting for the build, as requested by the user.

# Plan A: externally managed ChatGPT tokens

Status: deferred until the user has tested Plan B and explicitly elects to proceed. Reserved CPA branch: `codex/plan-a-external-auth`, based on `02a86349` with no implementation changes; future Codex fork branch: `codex/cpa-external-auth`. No Plan A runtime is enabled by Plan B. This design is kept with the Plan B documentation for comparison; do not merge Plan B implementation into Plan A merely to create its branch.

Reuse Plan B's versioned inference bridge, fidelity tests, worker isolation, cancellation, and release contract. Only authentication ownership differs: a single external authority supplies accessToken, chatgptAccountId, and optionally chatgptPlanType through official experimental `chatgptAuthTokens`. It must handle `account/chatgptAuthTokens/refresh`; the consuming worker does not possess or rotate the refresh token. In-memory auth must be reinstalled after a worker restart. This is not ordinary CLI `login --with-access-token`.

The authority can be CPA or a separate credential broker, but must be chosen explicitly. If Codex-owned OAuth is required, remain on Plan B. Preserve workspace/account binding during refresh; never substitute another account in a refresh response. Keep the callback connection alive, serialize refresh at the authority, and return failures without retry amplification.

Keep installation identity local to each persistent worker CODEX_HOME. External token injection neither clones another machine's installation nor disables agent tools. The bridge's inference-only boundary remains mandatory.

Acceptance: all Plan B inference/lifecycle/upgrade tests, plus expiry, refresh callback success/failure/disconnection, process restart, account mismatch, concurrent refresh deduplication, and absence of refresh-token exposure to workers.

Official references:
- https://developers.openai.com/codex/app-server
- https://developers.openai.com/codex/auth

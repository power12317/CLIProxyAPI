# Prism browser adapter

This is the local P0/P1 adapter used by CLIProxyAPI's Codex Prism route. It
listens on `127.0.0.1:8319`, accepts only the bridge key, and uses a sandboxed
Playwright Chromium session to submit one plain-text `gpt-5.6-sol` / `medium`
turn at a time.

The adapter is intentionally conservative. It does not implement tools,
WebSockets, compact, images, server-side conversations, token deltas, or
authoritative usage. An uncertain `start` or `status` result leaves a `pending`
journal entry and the next request is rejected until the result is inspected.

Required environment variables:

```text
PRISM_ADAPTER_API_KEY=<same secret configured outside config.yaml>
PRISM_ADAPTER_CHROME=/absolute/path/to/chromium
CHROME_DEVEL_SANDBOX=/absolute/path/to/chrome-sandbox
PRISM_ADAPTER_STATE_DIR=/var/lib/cliproxy-prism
```

Run it as a non-root user. Do not use `--no-sandbox`. The `/health` endpoint
only reports that the process is alive; it does not validate Prism OAuth or a
model request.

Install the pinned Playwright wheel from `requirements.txt` on the build or
deployment host. Do not put bridge keys, OAuth tokens, cookies, browser state,
or pending files in this repository.

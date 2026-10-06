# CLIProxyAPI Prism transport

This adapter implements the Prism behavior of sub2api production
`0ae36e501952000c5c910a2e616c6e0861f66a49`. Source provenance and licensing are in
[UPSTREAM.md](UPSTREAM.md).

## Routing and capabilities

CPA exposes one switch in network settings, disabled by default:

```yaml
codex:
  prism:
    enabled: false
```

Enabling the switch routes eligible Codex OAuth credentials through Prism for
the complete sub2api catalog: `gpt-6.1-sol`, `gpt-5.6-sol`, `gpt-5.6-terra`, and
`gpt-6-luna`. Routing applies after model alias and thinking-suffix resolution.
Other models retain their normal Codex/Basispoints route. Prism requests do not
fall back to another protocol on failure.

The switch registers these models even when the native Codex catalog has not
listed them yet. Hot reload updates the model list and scheduler without a
restart. OAuth login uses the existing flow.

Supported reasoning levels are low, medium, high, and xhigh (medium when omitted),
subject to the options actually available on the Prism account. Model/effort
selection is refreshed on every request. Other models and native WebSocket,
compact, image, background, and structured-output protocols are not implemented
by the upstream Prism adapter. WebSocket selection excludes accounts whose mapped
model uses Prism; other accounts remain available for WebSocket requests.

The client-tool bridge follows upstream: enabled by default for **gpt-6.1-sol**,
with function/custom tools, nested namespaces, additional_tools, tool_choice, and
parallel_tool_calls. Tools execute at the client. Subsequent requests carry the
complete call/result history and original tool catalog. Each tool turn uses a
fresh project, while caller/account/conversation identity and call consumption
are preserved. CPA forwards tools according to this upstream policy without a
separate tool setting. Both processes must be upgraded together.

Prism returns completed responses rather than token deltas. SSE includes completed
text/tool items and a final response. Usage remains null; CPA publishes
`usage_unavailable: true` with null tokens/token_breakdown to its usage queue while
retaining request outcomes and latency. No authoritative usage is estimated.

## Sessions and execution modes

Caller scope comes from authenticated CPA request context. Standard session/thread
headers and Codex client_metadata identify conversations, with thread identity
preferred over session identity. Client-supplied private X-Prism headers never
select another caller's state. Requests lacking explicit identity create a fresh
project. Text conversations reuse their project but open a new chat tab before
submitting full client history. Cache TTL is 300 seconds, maximum age 900 seconds,
and credential changes replace contexts after active work drains.

The adapter retains sub2api's default browser execution mode and optional
multiplex implementation. Multiplex defaults to 20 active requests, 20 per
account, 30 queued requests, and one project preparation at a time.

The upstream bounds are 30 active requests, 60 queued requests, and 1-2 preparation
pages. A conversation remains sequential; independent conversations may overlap.
The current multiplex entrypoint holds one account context at a time. It does not
claim to implement a multi-account browser pool. A waiting account receives busy
while the current context is occupied. The adapter's upstream deployment controls
remain internal to that process.

Multiplex hands polling from the temporary editor to a stationary official page,
preserves the official fetch/Sentinel flow and asset cache, and reclaims closed
editor resources. Memory pressure waits up to 30 seconds at the upstream 750 MiB
threshold; runtime-start throttling cools new starts for at least 60 seconds.
Already submitted work continues. Explicit pre-execution sandbox
reconnect follows the official page for up to three start attempts; unknown
outcomes are never automatically resubmitted. Pending records are per conversation.
Projects remain in the Prism workspace; upstream does not automatically delete them.

## Installation

Use the matching prebuilt Chromium runtime and a non-root service account:

```sh
cd prism-adapter
python3 -m venv venv
venv/bin/pip install --only-binary=:all: -r requirements.txt
venv/bin/python -m playwright install chromium
```

Configure the same random bridge key in the CPA process and adapter process.
See `../deploy/prism-adapter/prism-adapter.env.example` and the service unit.
The gateway uses the adapter's loopback endpoint and default port 8319. If a
deployment changes the adapter port, both processes must receive the same
`PRISM_ADAPTER_PORT` environment value. Browser paths and the bridge key belong to
deployment setup; the CPA YAML and management panel expose only the switch.
Provision the state directory before starting the service. On Linux the Chromium
sandbox helper may require its standard root-owned SUID setup. The service does
not disable Chromium sandboxing. If CPA runs in a container, the adapter must
share its network namespace for the loopback endpoint to work.

`/health` is process liveness, not a live model entitlement probe. Tokens, cookies,
state directories, and environment secrets must stay outside this repository.
The feature remains off until enabled through CPA configuration. No deployment
is performed by building this branch.

Prism is included in `main`. The gateway image is
`ghcr.io/power12317/cliproxyapi:latest` for Linux amd64 and arm64. Pushing a `v*`
Git tag publishes its version and `latest`. Branch-specific image publication
has been removed. The standalone adapter
still runs separately using the installation steps above and a shared loopback network.

## Verification

```sh
prism-adapter/venv/bin/python -B -m unittest discover -s prism-adapter -p 'test_*.py'
prism-adapter/venv/bin/python prism-adapter/smoke_browser.py --chrome /path/to/chromium
prism-adapter/venv/bin/python prism-adapter/smoke_client_tools.py --chrome /path/to/chromium
prism-adapter/venv/bin/python prism-adapter/smoke_multiplex.py --chrome /path/to/chromium \
  --concurrency 5 --model gpt-6.1-sol --effort xhigh --rounds 3 --reconnect-first
PRISM_TEST_PYTHON=/path/to/venv/bin/python go test ./internal/runtime/executor -run Prism -count=1
```

Run these commands from the repository root (adjust the virtualenv path as needed).
Fixtures use real Chromium and local simulated upstream responses, with no real
OAuth or Prism generation. They prove integration behavior, not real-account
model availability, upstream concurrency capacity, or model quality.

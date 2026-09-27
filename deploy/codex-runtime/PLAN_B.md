# Plan B: Codex-owned managed OAuth

Status: implementation on CPA branch `codex/plan-b-managed-auth`. Codex fork work belongs to its dedicated project chat and branch `codex/cpa-managed-auth`.

## Ownership

CPA accepts client protocols, selects an explicitly configured `codex-runtime` worker, normalizes request semantics, and translates response protocols. The Codex fork owns managed ChatGPT login, native credential storage, refresh, installation identity, upstream transports, required response processing, and its enabled background lifecycle. CPA stores only worker ID, Unix socket, expected ChatGPT account ID, model allowlist, and routing settings. No OAuth secrets or writable auth-file projection are shared.

Workers are separate long-running containers with private CODEX_HOME volumes. They share CPA's network namespace in production. A deployment operator manages containers; CPA does not need Docker socket access. Disabling a worker stops new CPA selections, without logging the worker out. Existing legacy Codex credentials must not share the worker's OAuth grant: separately log in, or explicitly retire the old credential before migration.

## Inference boundary

Use the versioned protocol in PROTOCOL.md. The fork adds a narrow inference entry point to the existing app-server runtime and reuses official request/transport/response code. It must not invoke agent turns, tools, shell, files, MCP, or automatic follow-up generation. Tool calls are returned to the external client. Preserve raw event information alongside internal parsed events, after required buffering/processing. Unsupported semantics fail explicitly.

CPA does not silently fall back to its old Codex executor or automatically replay an ambiguous bridge failure. No read, idle, or total inference timeout is imposed after connection establishment. Cancellation closes the request connection and must cancel only that request in the worker.

## Acceptance gates

1. Request fidelity: native Responses fields, instructions, tools, tool choice, reasoning, and unknown extensions are not silently discarded.
2. Response fidelity: raw stream events, terminal status, usage, and tool calls survive the IPC boundary; missing terminal/completion is an error.
3. No tool execution: mock upstream tool calls cannot cause process, filesystem, MCP, or follow-up inference activity.
4. Auth isolation: only Codex refreshes/writes managed credentials; CPA config and admin mutations cannot overwrite token storage.
5. Account isolation: capabilities and every inference validate expected credential/account; caller-scoped session IDs never cross callers or accounts.
6. Lifecycle: disconnected clients cancel requests without killing auth/background services; disabled/deleted configuration no longer schedules workers.
7. Upgrade: run contract tests for both repositories against a pinned upstream revision and pinned fork artifact. Never deploy unvalidated upstream main automatically.
8. Deployment: one private data volume per credential, shared CPA network, no exposed unauthenticated TCP control API.

## Staging and limits

The first integration targets Responses HTTP/SSE and synchronous collection, with cross-protocol adaptation supplied by CPA. Compaction and persistent upstream sessions are capability-gated. No capability may be advertised merely because the ordinary CLI supports it. Realtime, arbitrary management APICall, special search/image endpoints, and Home dispatch require separate endpoint contracts; they cannot silently use legacy transport with managed runtime credentials. Home mode is rejected for this configuration until its owner/dispatch contract is implemented in both projects.

Real OAuth login and a real-account smoke test are operator steps; automated tests use synthetic credentials and local mock upstreams.

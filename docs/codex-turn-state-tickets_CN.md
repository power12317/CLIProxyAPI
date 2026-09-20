# Codex `x-codex-turn-state` 门票

CLIProxyAPI 可以主动获取 ChatGPT Codex OAuth 账号使用的、有效期约一小时的
`x-codex-turn-state` 值。此功能默认关闭。启用后，后台 harvester 会通过
`codex.turn-state-ticket.harvest-proxy-url`，为每个处于活动状态的 Codex OAuth
凭据和配置模型发送探测请求。正常业务请求仍然使用每个账号自身配置的代理。

## 配置示例

```yaml
codex:
  turn-state-ticket:
    enabled: true
    target-length: 292
    ttl-seconds: 3600
    refresh-before-seconds: 600
    harvest-proxy-url: "socks5://user:password@residential.example:1080"
    probe-interval-seconds: 6
    attempt-timeout-seconds: 25
    fail-closed: true
    models:
      - gpt-6-astra
      - gpt-5.6-sol
```

参数含义如下：

- `enabled`：是否启用主动获取门票。默认是 `false`。
- `target-length`：接受的门票长度。默认是 `292`。
- `ttl-seconds`：门票在本地保存的有效期，默认是 `3600` 秒。
- `refresh-before-seconds`：距离过期少于此秒数时重新探测，默认是 `600` 秒。
- `harvest-proxy-url`：专门用于获取门票的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理。
  业务请求不会改用这个代理。
- `probe-interval-seconds`：两轮后台探测之间的间隔，默认是 `6` 秒。
- `attempt-timeout-seconds`：单次探测的最长时间，默认是 `25` 秒。
- `fail-closed`：没有有效门票时是否阻断对应模型请求。默认是 `true`。
- `models`：需要主动获取门票的模型列表。默认是 `gpt-6-astra` 和 `gpt-5.6-sol`。

如果代理包含用户名和密码，请确保密码已经正确 URL 编码。不要把带有真实密码的
配置文件提交到代码仓库或发送给其他人。

## 获取和续期规则

只有同时满足以下条件的上游响应才会被接受为门票：

- HTTP 状态码是 `200`。
- 响应头中的 `X-Codex-Turn-State` 值以 `gAAAAA` 开头。
- 值的长度等于 `target-length`，默认是 `292`。

门票按照“账号 ID + 模型”分别保存到 auth metadata 中。每张门票都会记录获取时间、
过期时间和长度。后台 harvester 会跳过仍然有效且没有进入续期窗口的门票；进入续期
窗口后才会重新探测。

探测失败不会删除上一张仍然有效的门票。这样可以避免一次临时的网络问题立即影响
正常请求。没有门票时，系统会继续在下一轮探测中重试。

## `fail-closed` 行为

当 `fail-closed: true` 时，配置列表中的模型必须先有有效门票才能发往上游。如果
当前选中的账号没有门票，executor 会返回可重试的“门票不可用”错误，账号调度器可以
继续尝试其他符合条件的凭据。

当 `fail-closed: false` 时，没有门票的请求仍然会继续发送。此时系统不会主动注入
门票，但现有的被动 turn-state 缓存仍然保留，能够继续学习和回放正常业务响应中观察
到的 `x-codex-turn-state`。

## 管理 API

管理端提供以下接口：

```text
GET   /v0/management/codex-turn-state-ticket
PUT   /v0/management/codex-turn-state-ticket
PATCH /v0/management/codex-turn-state-ticket
```

`GET` 返回当前策略和账号级状态，包括：

- 是否启用。
- 门票长度、有效期和提前续期时间。
- 探测间隔和单次探测超时。
- `fail-closed` 状态。
- 目标模型列表。
- harvest proxy 是否已配置，以及已脱敏的代理地址。
- 每个 Codex OAuth 账号、模型的 `ready`、长度、剩余秒数、过期时间和阻断状态。

`PUT` 和 `PATCH` 可以更新这些策略。提交管理 API 返回的脱敏代理地址时，系统会
保留服务端已经保存的代理密码；提交新的完整代理 URL 时，系统会先校验协议、主机、
端口以及不能包含路径、查询参数或片段。

`/v0/management/auth-files` 的账号条目在功能启用时也会包含
`codex_turn_tickets` 摘要。摘要只包含状态信息，不包含门票内容。

## 敏感信息处理

门票是短期运行凭据，系统不会把它当作普通账号信息对外暴露：

- 管理 API 不返回门票 blob。
- `GET /v0/management/config` 会隐藏 harvest proxy 中的密码。
- 下载 auth 文件时会移除 `codex_turn_ticket:*` 字段。
- 账号列表只返回门票是否就绪、长度、剩余时间和过期时间。
- 账号中的其他 OAuth token 和普通 metadata 不会因为门票脱敏而被删除。

## 不同 Codex 传输方式的处理

HTTP、SSE 和 WebSocket Codex 请求都会在普通请求头完成后注入门票。

WebSocket 连接复用时不会为每个请求重新执行 HTTP handshake，因此系统还会把门票
同步写入每个请求体的：

```text
client_metadata.x-codex-turn-state
```

这样同一条复用连接上的后续请求也能携带正确的门票。`responses/compact` 同样使用
相同的账号、模型和有效期规则。

系统原有的被动 turn-state 缓存仍然保留。没有配置主动门票的模型，以及主动门票关闭
时的兼容请求，继续按照原有响应观察和回放流程处理。

## 前端管理页面

CPA-Manager-Plus 的 `Codex Turn-State` 页面提供以下操作：

- 开启或关闭主动门票获取。
- 开启或关闭 `fail-closed`。
- 修改 harvest proxy。
- 修改需要主动获取门票的模型列表。
- 按账号和模型查看门票是否就绪。
- 查看门票剩余有效时间和阻断状态。
- 手动刷新状态。

页面不会显示门票原文，代理密码在保存后也会继续保持掩码。

# Codex `x-codex-turn-state` 门票

CLIProxyAPI 可以主动获取 ChatGPT Codex OAuth 账号使用的、有效期约一小时的
`x-codex-turn-state` 值。此功能默认关闭。启用后，后台 harvester 会为每个处于活动状态的
Codex OAuth 凭据和配置模型发送探测请求。配置了
`codex.turn-state-ticket.harvest-proxy-url` 时使用该代理；留空时直接请求。正常业务请求仍然
使用每个账号自身配置的代理。

## 配置示例

```yaml
codex:
  turn-state-ticket:
    enabled: true
    ttl-seconds: 3600
    refresh-before-seconds: 600
    harvest-proxy-url: "socks5://user:password@residential.example:1080"
    probe-interval-seconds: 60
    attempt-timeout-seconds: 25
    fail-closed: true
    models:
      - gpt-6-astra
      - gpt-5.6-sol
```

参数含义如下：

- `enabled`：是否启用主动获取门票。默认是 `false`。
- 门票目标长度不再由全局配置决定，而是按账号属性严格选择：Personal（`free`、`plus`、`pro`）使用 `292`，Team/Business（`team`、`business`）使用 `332`。未知属性按 Personal 的 `292` 处理。
- `ttl-seconds`：门票在本地保存的有效期，默认是 `3600` 秒。
- `refresh-before-seconds`：距离过期少于此秒数时重新探测，默认是 `600` 秒。
- `harvest-proxy-url`：可选的门票获取代理，支持 HTTP、HTTPS、SOCKS5 或 SOCKS5H。
  留空时直接探测；业务请求不会改用这个代理。
- `probe-interval-seconds`：两轮后台探测之间的间隔，默认是 `60` 秒（1 分钟）。省略或设置为非正数时使用默认值；已经显式配置的正数继续生效，旧配置中的 `6` 需要改为 `60` 才会按分钟探测。
- `attempt-timeout-seconds`：单次探测的最长时间，默认是 `25` 秒。
- `fail-closed`：没有有效门票时是否阻断对应模型请求。默认是 `true`。
- `models`：需要主动获取门票的模型列表。默认是 `gpt-6-astra` 和 `gpt-5.6-sol`。

如果代理包含用户名和密码，请确保密码已经正确 URL 编码。不要把带有真实密码的
配置文件提交到代码仓库或发送给其他人。

## 获取和续期规则

只有同时满足以下条件的上游响应才会被接受为门票：

- HTTP 状态码是 `200`。
- 响应头中的 `X-Codex-Turn-State` 值以 `gAAAAA` 开头。
- Personal 账号的值长度是 `292`，Team/Business 账号的值长度是 `332`。

门票按照“账号 ID + 模型”分别保存到 auth metadata 中。账号的 `plan_type` 决定该账号的目标长度。每张门票都会记录获取时间、
过期时间和长度。后台 harvester 会跳过仍然有效且没有进入续期窗口的门票；进入续期
窗口后才会重新探测。

正常 Codex 请求返回目标长度的门票时，系统会直接按账号和模型保存并复用这个值：Personal
保存 292，Team/Business 保存 332。它会一直复用到距离本地过期少于 10 分钟，或者 Personal
主动门票请求收到 312 值而被立即失效。返回其他长度的响应不会提升为主动门票，当前 turn
仍沿用原有按 `turn_id` 的被动缓存；只有后续探测或正常响应再次拿到目标长度，才会持久替换。
探测失败不会删除上一张仍然有效的门票。没有有效门票时，后台会继续按探测间隔重试。

## `fail-closed` 行为

当 `fail-closed: true` 时，配置列表中的模型必须先有有效门票才能发往上游。如果
当前选中的账号没有门票，executor 会返回可重试的“门票不可用”错误，账号调度器可以
继续尝试其他符合条件的凭据。

当 `fail-closed: false` 时，没有有效主动门票的请求仍然会继续发送，并沿用当前请求已有的
按 `turn_id` 被动缓存。只要获取到符合账号属性的有效门票（Personal 为 292，Team/Business
为 332），即使 `fail-closed` 是 `false`，也仍然会强制使用这张门票替换请求中的 turn-state，
并独立于 `turn_id` 持续复用。没有有效门票时不会把非目标长度的响应持久化成主动门票。

如果 ChatGPT 在有效的 Personal 292 门票请求后返回长度为 312 的 `x-codex-turn-state`，CPA
会立即让对应账号和模型的门票失效，删除本地持久化值，并启动一次新的主动探测。下
一个请求在 `fail-closed: true` 时会等待新的有效门票；在 `fail-closed: false` 时会
继续发送，但不会使用旧的 Personal 292 门票。

## 管理 API

管理端提供以下接口：

```text
GET   /v0/management/codex-turn-state-ticket
PUT   /v0/management/codex-turn-state-ticket
PATCH /v0/management/codex-turn-state-ticket
```

`GET` 返回当前策略和账号级状态，包括：

- 是否启用。
- Personal/Team/Business 对应的门票目标长度、有效期和提前续期时间。
- 探测间隔和单次探测超时。
- `fail-closed` 状态。
- 目标模型列表。
- harvest proxy 是否已配置，以及已脱敏的代理地址。
- 每个 Codex OAuth 账号、模型的目标长度、实际长度、`ready`、剩余秒数、过期时间和阻断状态。

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

系统原有的被动 turn-state 缓存仍然保留。没有配置主动门票的模型，以及尚未获得目标长度
门票时的请求，继续按照原有响应观察和回放流程处理。配置了有效主动门票的模型则按照
“账号 + 模型”的门票处理，后续请求不再根据 `turn_id` 选择另一个主动门票值。

## 日志与请求监控的长度统计

访问日志的 `请求长度/响应长度` 与请求监控中的 `request_turn_state_len`、
`response_turn_state_len` 使用同一份统计；监控队列中的 `turn_state_len` 由这两个值组成。
请求长度应当在主动门票完成强制替换后记录，HTTP/SSE/compact 读取最终请求头，WebSocket
优先读取当前发送帧中的门票值，复用连接时也会为每个请求更新。

此前在主动门票注入前记录请求长度，会出现实际发送 292、日志和监控却显示 `0/0`
的情况。本次修正统计时机后，新请求的显示对应如下：

| 实际请求与响应 | 日志与监控显示 |
|---|---|
| 已注入 Personal 292，响应未返回 turn-state | `292/0` |
| 已注入 Team/Business 332，响应未返回 turn-state | `332/0` |
| 被动缓存 312 被主动 292 覆盖，响应未返回 turn-state | `292/0` |
| 请求未携带 turn-state，响应返回 312 | `0/312` |

这项修正仅更新观测数据，不改变门票获取、有效期、凭证隔离、注入或被动缓存规则。
更新后新产生的日志和请求监控记录生效，已有历史记录不会重写。

## 前端管理位置

CPA-Manager-Plus 按照账号凭证的归属展示门票状态：

- 凭证管理的账号列表按账号显示每个配置模型的门票是否就绪、剩余有效时间和阻断状态。
- 账号详情抽屉显示该账号的完整模型门票状态。
- 全局配置页管理主动获取开关、`fail-closed`、harvest proxy 和模型列表。
- 旧的 `/codex-turn-state` 地址保留兼容跳转到凭证管理，不再作为独立资源页面。

前端不会显示门票原文，代理密码在保存后也会继续保持掩码。

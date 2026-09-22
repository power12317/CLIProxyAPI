# Codex `x-codex-turn-state` 门票

CLIProxyAPI 可以主动获取 ChatGPT Codex OAuth 账号使用的、有效期约一小时的
`x-codex-turn-state` 值。此功能默认关闭。启用后，后台 harvester 会为每个处于活动状态的
Codex OAuth 凭据和配置模型发送探测请求。配置了
`codex.turn-state-ticket.harvest-proxy-url` 时使用该代理；留空时直接请求。正常业务请求仍然
使用每个账号自身配置的代理。总开关开启后，`cache-all-models` 默认开启，所有模型的正常
响应都可以保存符合账号属性的票据并持续注入；主动探测仍只针对 `models` 中的模型。

## 配置示例

```yaml
codex:
  turn-state-ticket:
    enabled: true
    cache-all-models: true
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

- `enabled`：门票功能总开关，控制主动探测、正常响应保存及跨 turn 强制注入。默认是 `false`。
- `cache-all-models`：是否对所有模型的正常响应保存门票并跨 turn 强制注入。默认是 `true`，省略等同于开启；只在 `enabled: true` 时生效。关闭后只处理 `models` 中的模型。
- 门票目标长度不再由全局配置决定，而是按账号属性严格选择：Personal（`free`、`plus`、`pro`）使用 `292`，Team/Business（`team`、`business`）使用 `332`。未知属性按 Personal 的 `292` 处理。
- `ttl-seconds`：门票在本地保存的有效期，默认是 `3600` 秒。
- `refresh-before-seconds`：主动探测列表中的模型距离过期少于此秒数时重新探测，默认是 `600` 秒。列表外的模型不主动续期。
- `harvest-proxy-url`：可选的门票获取代理，支持 HTTP、HTTPS、SOCKS5 或 SOCKS5H。
  留空时直接探测；业务请求不会改用这个代理。
- `probe-interval-seconds`：一轮后台探测结束后，到下一轮开始的等待时间，默认是 `60` 秒（1 分钟）。启动和从关闭切换为开启时立即执行首轮，不先等这个间隔。省略或设置为非正数时使用默认值；已经显式配置的正数继续生效，旧配置中的 `6` 需要改为 `60` 才会按分钟探测。
- `attempt-timeout-seconds`：单次探测的最长时间，默认是 `25` 秒。
- `fail-closed`：主动探测列表中的模型没有有效门票时是否阻断请求。默认是 `true`。列表外的模型始终可以正常请求以取得票据。
- `models`：需要主动获取门票的模型列表。默认是 `gpt-6-astra` 和 `gpt-5.6-sol`。开启 `cache-all-models` 不会扩充这个列表。

如果代理包含用户名和密码，请确保密码已经正确 URL 编码。不要把带有真实密码的
配置文件提交到代码仓库或发送给其他人。

## 所有模型的正常响应缓存

只需保留 `enabled: true`，省略 `cache-all-models` 就会启用此行为；也可以显式配置为 `true`。
例如，`models` 只配置 Astra、Sol，同一邮箱有 Mac、Windows 两份凭据：

| 模型 | 是否主动探测 | 正常响应取得目标长度后的行为 |
|---|---|---|
| `gpt-6-astra`、`gpt-5.6-sol` | 是 | 保存并注入，进入续期窗口后可主动刷新 |
| `gpt-5.6-luna`、`gpt-5.6-terra` 等其他模型 | 否 | 保存并注入，直到过期或按现有规则失效 |

在这些票据均缺失且探测均成功时，首轮主动探测仍为两份凭据 × 两个配置模型，共 4 次，
不会因正常请求使用 Luna、Terra 变成 8 次。取得票据后以“凭据 ID + 模型”独立持久化：
Mac 与 Windows 不共用，Luna 与 Terra 也不共用。同一凭据、同一模型的后续请求会强制使用
该票据，覆盖原请求或被动缓存中的 turn-state，不受 `turn_id` 是否变化影响。

列表外模型的有效期也沿用 `ttl-seconds`，默认从取得响应值时起一小时。后续只注入已有
票据不会延长有效期；正常响应再次取得目标长度的值时，会立即替换并重新计算有效期。
即使进入最后 10 分钟，也不会为列表外模型增加主动探测。到期后停止强制注入，等待后续
正常响应取得新票据；Personal 账号正常响应返回 `312` 时，同样删除对应缓存但不触发探测。
没有票据时沿用原有按 `turn_id` 的被动缓存，`312` 不会成为跨 turn 持续注入的票据。

| 总开关 `enabled` | 子开关 `cache-all-models` | 行为 |
|---|---|---|
| `false` | 任意值 | 门票探测、保存和强制注入均关闭，保留原有被动缓存行为 |
| `true` | 省略或 `true` | 配置模型主动探测；所有模型正常响应都可保存及强制注入 |
| `true` | `false` | 只有配置模型参与票据保存、强制注入及主动探测 |

关闭子开关不会删除已持久化的其他模型票据，但这些票据不再强制注入，也不再加入管理接口的
票据摘要；重新开启时，尚未过期的票据可以继续使用。原有 Personal `292`、Team/Business
`332` 的长度规则保持不变。

## 探测请求的设备标识和输入

探测向 `https://chatgpt.com/backend-api/codex/responses` 发送 `POST` 请求，使用当前凭据的
access token。输入文本为 `ping`，请求体结构如下（标识符为占位示例）：

```json
{
  "model": "gpt-6-astra",
  "store": false,
  "stream": true,
  "instructions": "Reply with exactly: pong",
  "input": [
    {
      "role": "user",
      "content": [{ "type": "input_text", "text": "ping" }]
    }
  ],
  "client_metadata": {
    "session_id": "本次探测的会话 UUID",
    "x-codex-installation-id": "固定设备 UUID",
    "x-codex-turn-metadata": "{\"installation_id\":\"固定设备 UUID\",\"session_id\":\"本次探测的会话 UUID\",\"turn_id\":\"本次探测的轮次 UUID\"}"
  }
}
```

开启 **Codex 设备固定**（`codex.device-convergence: true`，省略时默认开启）后，探测和正常
对话使用相同的设备 ID 生成规则：以凭据的 `account_id` 和 `codex_client_system` 为依据，
缺少账号 ID 时回退到凭据自身 ID；系统属性缺省按 Mac 处理。同一凭据探测不同模型、
重复探测或重启服务时，设备 ID 均保持一致；同一账号的 Mac 和 Windows 凭据使用不同的设备 ID。
设备 ID 不改变门票按“凭据 ID + 模型”独立保存的规则。

固定设备 ID 同时写入 `client_metadata.x-codex-installation-id` 和
`client_metadata.x-codex-turn-metadata` 内的 `installation_id`。
HTTP 请求头 `X-Codex-Turn-Metadata` 与请求体里的这份 JSON 字符串完全一致。
每次探测的 `session_id` 和 `turn_id` 仍分别生成新的 UUID。

关闭设备固定时，探测不添加上述两个设备 ID 字段。输入 `ping` 和请求头、请求体之间的
turn metadata 同步继续生效。响应指令仍为 `Reply with exactly: pong`；探测只读取响应头
中的票据，不等待或读取模型生成的回答。

## 获取和续期规则

主动探测只有同时满足以下条件的上游响应才会被接受为门票：

- HTTP 状态码是 `200`。
- 响应头中的 `X-Codex-Turn-State` 值以 `gAAAAA` 开头。
- Personal 账号的值长度是 `292`，Team/Business 账号的值长度是 `332`。

门票按照“凭据 ID + 模型”分别保存到 auth metadata 中。账号的 `plan_type` 决定该账号的目标长度。每张门票都会记录获取时间、
过期时间和长度。后台 harvester 只检查配置列表中的模型，跳过仍然有效且没有进入续期窗口的
门票；进入续期窗口后才会重新探测。

正常 Codex 请求返回以 `gAAAAA` 开头的目标长度门票时，系统会直接按凭据和模型保存并复用这个值：Personal
保存 292，Team/Business 保存 332。票据会一直复用到本地过期、被新票据替换，或者 Personal
请求收到 312 值而被立即失效。配置模型在剩余 10 分钟时尝试提前续期，期间旧票据仍可使用。
返回其他长度的响应不会提升为主动门票，当前 turn
仍沿用原有按 `turn_id` 的被动缓存；只有后续探测或正常响应再次拿到目标长度，才会持久替换。
探测失败不会删除上一张仍然有效的门票。配置模型没有有效门票时，后台会继续按探测间隔重试。

探测统一按顺序执行，同一时刻最多有一个主动探测请求。保存结果时，只把本次模型的票据变更合并到最新凭据，
其他模型的票据以及期间更新的 access token、备注等字段继续保留。正常响应取得门票和
票据失效删除也使用相同的增量合并方式。

此前使用探测开始时的整份凭据快照覆盖保存，可能让先完成模型的新票据被后完成模型的
旧快照覆盖：原来没有票据就显示“缺失”，原来有过期 292 就显示“实际 292”但未就绪。
增量合并修复后，两个模型取得的新票据会同时保留，写入文件和重载后的管理接口均能读取。

## 启动、开启开关和账号探测顺序

- 服务启动时，如果开关已经开启，立即执行一轮探测；运行中从关闭切换为开启时，配置生效后立即唤醒探测，不等原来的间隔计时结束。
- 立即执行的是一轮检查和必要的探测：仍有有效票据且未进入提前续期窗口的模型会跳过。
- 凭据按 ID 排序，通常就是凭据文件名顺序；每份凭据按配置中的 `models` 顺序探测。上一个请求结束并保存结果后才发下一个，不同时发出 Mac、Windows 或不同模型的探测请求。
- 任一探测响应的 `x-codex-turn-state` 长度为 `312`，立即暂停这个账号本轮所有剩余探测。从收到该响应起，至少等待一个 `probe-interval-seconds`，再在后续探测轮次重试。即使响应状态不是 `200`，返回长度为 `312` 也执行暂缓。
- 同账号按凭据 metadata 中的 `email` 或 `account_id` 识别：邮箱相同或账号 ID 相同就共同暂缓。比较时去掉首尾空格并忽略大小写；只有邮箱、只有账号 ID 的凭据也会与已知关联凭据一并暂停。两个字段都没有时才按凭据 ID 区分。
- 其他账号继续按顺序探测。探测收到 `312` 不删除之前仍有效的票据，也不会把 `312` 保存为主动票据。
- 正常请求触发的补票进入同一个顺序调度，不能绕过账号暂缓；重复配置重载或关闭后再开启，也不会跳过尚未结束的暂缓时间。正常响应取得有效 `292`/`332` 时仍可直接保存。
- 关闭开关后，已发出的探测完成后不再发送队列中的下一个请求；停止服务会取消正在执行的探测。

例如，同一邮箱有 Mac、Windows 两份凭据，每份都配置了 Astra 和 Sol：Mac 的 Astra
取得 `292` 后继续探测 Mac 的 Sol；如果 Sol 返回 `312`，本轮就跳过 Windows 的两个模型。
下一轮到达且暂缓时间已过时，Mac 的 Astra 若仍有效就跳过，从仍需票据的模型继续。

这里共享的是探测暂缓状态。票据原文仍按“凭据 ID + 模型”独立保存和使用，Mac 与 Windows
凭据之间不共用票据，已有的设备固定逻辑也继续生效。

## `fail-closed` 行为

当 `fail-closed: true` 时，配置列表中的模型必须先有有效门票才能发往上游。如果
当前选中的账号没有门票，executor 会返回可重试的“门票不可用”错误，账号调度器可以
继续尝试其他符合条件的凭据。

当 `fail-closed: false` 时，没有有效主动门票的请求仍然会继续发送，并沿用当前请求已有的
按 `turn_id` 被动缓存。只要获取到符合账号属性的有效门票（Personal 为 292，Team/Business
为 332），即使 `fail-closed` 是 `false`，也仍然会强制使用这张门票替换请求中的 turn-state，
并独立于 `turn_id` 持续复用。没有有效门票时不会把非目标长度的响应持久化成主动门票。

如果 ChatGPT 在有效的 Personal 292 门票请求后返回长度为 312 的 `x-codex-turn-state`，CPA
会立即让对应凭据和模型的门票失效，删除本地持久化值。配置模型会将补票加入顺序调度；若该账号
处于探测 `312` 暂缓期，则等后续探测轮次再执行。下
一个请求在 `fail-closed: true` 时会等待新的有效门票；在 `fail-closed: false` 时会
继续发送，但不会使用旧的 Personal 292 门票。列表外模型不受 `fail-closed` 阻断，也不加入
补票调度，后续正常请求取得目标长度时再保存。

## 管理 API

管理端提供以下接口：

```text
GET   /v0/management/codex-turn-state-ticket
PUT   /v0/management/codex-turn-state-ticket
PATCH /v0/management/codex-turn-state-ticket
```

`GET` 返回当前策略和账号级状态，包括：

- 是否启用。
- `cache_all_models`：是否对所有模型保存和注入正常响应票据，省略配置时返回 `true`。
- Personal/Team/Business 对应的门票目标长度、有效期和提前续期时间。
- 探测间隔和单次探测超时。
- `fail-closed` 状态。
- 主动探测模型列表。
- harvest proxy 是否已配置，以及已脱敏的代理地址。
- 每个 Codex OAuth 账号、模型的目标长度、实际长度、`ready`、剩余秒数、过期时间和阻断状态。

`PUT` 和 `PATCH` 可以更新这些策略。提交管理 API 返回的脱敏代理地址时，系统会
保留服务端已经保存的代理密码；提交新的完整代理 URL 时，系统会先校验协议、主机、
端口以及不能包含路径、查询参数或片段。

新开关在 YAML 中使用 `cache-all-models`，在上述管理接口中使用 `cache_all_models`。
例如 `PATCH /v0/management/codex-turn-state-ticket` 提交 `{"cache_all_models": false}` 可关闭
全模型缓存；提交 `true` 可开启。更新时不提交这个字段会保留原值。

`/v0/management/auth-files` 的账号条目在功能启用时也会包含
`codex_turn_tickets` 摘要。两个接口均先返回配置模型，再返回已保存票据的其他模型（按模型名排序）。
因此正常请求取得 Luna、Terra 票据后，摘要也包含其长度、是否就绪及剩余时间；从未保存票据的
列表外模型不产生摘要项，列表外模型的 `blocked` 始终为 `false`。摘要只包含状态信息，不包含门票内容。

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

系统原有的被动 turn-state 缓存仍然保留。尚未获得目标长度门票，或关闭全模型缓存后请求
列表外模型时，继续按照原有响应观察和回放流程处理。有符合策略的有效门票时则按照
“凭据 ID + 模型”强制注入，后续请求不再根据 `turn_id` 选择另一个主动门票值。

## 主动探测请求日志

每次实际探测请求结束后，运行日志输出一条 `[TICKET-PROBE]` 记录。正常响应在 `info`
级别可见，格式如下（时间、凭证和标识符为示例）：

```text
[2026-09-20 18:02:00] [a1b2c3d6] [codex-user-team-windows.json] [7a120003] [8b230003] [info ] [TICKET-PROBE] 200 | 1.420s | gpt-5.6-sol/- | 0/332 | POST "/backend-api/codex/responses"
[2026-09-20 18:03:00] [a1b2c3d7] [codex-user-pro.json] [7a120004] [8b230004] [info ] [TICKET-PROBE] 200 | 1.105s | gpt-6-astra/- | 0/312 | POST "/backend-api/codex/responses"
```

前缀依次为时间、独立探测请求 ID、实际凭证文件、探测 session 和 turn 的前 8 位、日志级别、
探测标记。正文依次记录真实 HTTP 状态、请求耗时、模型、请求与响应票据长度、方法和上游路径。
当前探测只读取响应头，未取得响应模型时显示 `-`；返回 312 即使未被保存也照实记录为 312。
没有响应票据时长度为 0；网络失败没有 HTTP 响应时状态显示 `---`。耗时截止于收到响应头或请求失败。
仅记录票据长度，不输出原始票据或 access token。有效票据无需探测时不产生这类请求记录。

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

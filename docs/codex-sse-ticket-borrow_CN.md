# 跨实例 SSE 借票：第一阶段实现与测试

本功能已合并到 `codex/oailb-borrow`，覆盖该分支的旧借票实现。实现最初在
`codex/sse-ticket-borrow` 开发，基于 CPA 主线 `e50b3414`。旧分支历史作为合并父提交保留。
实现参考 `ranxi2001/sub2api` 的借种子票、两次探针验证及路由 Cookie 配对方法；参考代码为
`production` 提交 `0d1f0359`（相关借票代码与 `v2.10.7` 一致）。
跨实例配置和管理密钥加密沿用 CPA `codex/oailb-borrow` 的方案。

本次只修改 **CLIProxyAPI**：A 与 B 均部署本分支版本。A 使用 Codex 模式供票时，需要搭配已经通过现有 v3 通道回传原始头的 codex-server；本次未修改其他项目或新增前端页面。
这是用户确认的一小时来源票实验：A 的来源票按一小时判断，B 换出的目标票按四分钟判断，
并未照搬参考项目对来源票也使用四分钟的规则。

## 1. 适用范围

- A 是正常来源账号所在的 CPA；B 是需要借票的账号所在的 CPA。
- 支持 Go 直接连接 `https://chatgpt.com/backend-api/codex/responses` 的 SSE 路径。
  下游请求流式或非流式均可，两者向上游使用 SSE。
- B 的借票执行继续只支持直连 SSE，参与借票的请求不使用 WebSocket、Codex Server/runtime 或 Basispoints 通道。
- A 的来源既支持直连 SSE，也支持 Codex Server/runtime v3：后者从正常请求的现有上游通知与 SSE 元数据保存配套票据。
  compact、独立图片接口不作为来源采集入口。
- 不引入中央票据池、自动寻找来源账号、主票/备用票双池或自动更换出口。
- B 的两次探针和业务请求使用 B 自己的 OAuth 身份、凭据出口及固定设备标识。
  A 的 OAuth token 不会借给 B。

## 2. A 的一小时来源快照

每份快照按“凭据 ID + 所属身份指纹 + 实际模型”仅保存在 CPA 进程内存中，包含 `x-codex-turn-state`、`__cflb`、`__oailb`、
所属凭据指纹、票据原始时间、到期时间，以及 Cookie 可解析的期限。

1. 票据总开关打开时，启动或从关闭切换为开启会立即检查并补采必要快照。
2. 配置模型依次采集，有完整有效且尚未进入续期窗口的快照就跳过。保留原来的到期前十分钟主动续期。
3. 来源票正常期限固定为原始签发时间加 3600 秒。解析不了签发时间的历史不透明票仍可按原机制本地保持，
   但不能导出为本轮可借组合。可解析的历史记录会受原始签发时间加一小时上限约束。
4. 同一张票再次出现、读取、借出、保存和配置重载都不能延长原期限。进程重启后缓存清空，不从 auth JSON 恢复。
5. 不同的新票与本次交换对应的 Cookie 一起形成新快照。不会在 B 取票时临时从最新 Jar 拼接。
6. 票据响应缺少 Cookie 时，可以继承该次请求实际发送的 Cookie；明确删除、缺失或过期的 Cookie 不可借。
7. 如果新响应无法形成完整组合，已有完整且有效的来源快照继续提供。
8. Cookie 更早失效时，会在定时检查中补采；旧版仅有票据而没有 Cookie 快照的记录也需要补采。
9. `cache-all-models` 的正常响应采集仍保留，主动采集列表不会因此扩大。

因此，A 在 10:00 生成的快照只要票与两枚 Cookie 仍有效，且 A 尚未更换这份快照，10:50 仍可重复借出。
如果 A 提前续期已经取得新组合，B 下次读取的是新的当前快照。
借出不消费票据，不记录“B 已用过此 A 票”，也不会触发 A 额外探测。

来源有效期固定为 **3600 秒**，旧 `ttl-seconds` 输入保留兼容，但不会再改变这个期限。
`refresh-before-seconds` 保留原有含义和配置，默认 **600 秒**：到期前十分钟进入主动续期窗口。
后台扫描/失败重试间隔仍由 `probe-interval-seconds` 控制，默认 60 秒。
在续期窗口中取得不同新票与配套 Cookie 后立即替换；返回同一张票不会延长它的原始一小时期限，
续期失败也保留尚未过期的旧票。到期后不再导出旧快照，等待下一次采集成功。
例如 10:00 签发的 A 票原定 11:00 到期，10:50 起按原来的定时检查主动续期；B 的借票动作不会额外触发 A 打票。

## 3. B 的请求触发流程

```mermaid
flowchart TD
    R[业务请求到达 B] --> C{当前 B 组合已验证且剩余大于 60 秒?}
    C -->|是| U[注入 B 票和对应两枚 Cookie]
    C -->|否| A[读取 A 当前模型的一小时快照]
    A --> P1[B 自己的身份 + A 票 + A Cookie：第一次探针]
    P1 --> N[取得不同于 A 种子的 B 新票及完整 Cookie 对]
    N --> P2[携带 B 新票和 Cookie 对：第二次续接探针]
    P2 --> V{HTTP 成功、SSE 完整、票及路由未变化?}
    V -->|是| S[缓存 B 组合：签发时间 + 240 秒]
    S --> U
    V -->|否| F{旧 B 组合仍有效且未被撤销?}
    A -->|获取失败| F
    P1 -->|取票失败| F
    F -->|是| U
    F -->|否| E[返回 503 借票不可用，后续请求可重试]
    U --> B[发送原业务请求一次]
    B --> O{响应是否返回不同票据或改变 Cookie 对?}
    O -->|否| K[继续保留原期限]
    O -->|是| I[撤销当前验证状态，下次业务请求再借票]
```

B 的缓存按“来源配置 + B 凭据身份 + 实际模型 + 出口”隔离。
Mac 与 Windows 两份凭据分别缓存，模型不同也分别缓存。

| 状态 | 当前业务请求的行为 |
|---|---|
| 剩余时间 > 60 秒 | 直接复用，不访问 A，不发探针 |
| 剩余 0～60 秒但未过期 | 先尝试续借；成功立即切换，失败可使用仍有效的旧组合 |
| 没有票、已到期、被撤销 | 先借票并验证；失败返回可重试错误，不使用旧 turn 缓存或一小时普通票 |
| 没有业务请求 | 不借票、不验证、不后台续借 |

一小时总开关和 `oailb-borrow` 同时启用时，B 不运行原本的后台模型打票。
既有独立的 usage/Cookie 预热功能不属于借票任务，仍保持原行为；它更新 Jar 也不能替换选定的借票 Cookie 对。

B 的期限从 **B 新票自身的签发时间** 开始计 240 秒；两次验证耗时包含在这四分钟中。
Cookie 如果更早到期，使用更早的期限。验证完成时还必须留有超过 60 秒的可用窗口。
相同 `turn_id` 也重新检查期限，过期后不会恢复被动缓存中的旧票。
已经发出的 SSE 继续正常输出，不会因为本地四分钟到期被中断或重放。

获取失败后约 30 秒内，后续请求不重复发起获取；这是请求触发的节流，没有后台重试。
同一凭据、模型的并发请求共用一轮获取和两次验证。

## 4. 两次探针的判断

探针仍为 `ping` / `Reply with exactly: pong`，使用既有固定设备 ID 规则。
请求体完成内建设置后才应用用户 payload 规则，之后不再恢复被过滤字段。
如果探针规则改变了所需模型、SSE 设置或加入冲突票据，会明确失败。

- 第一发使用 A 的种子票和配套 Cookie；必须返回一张新的 780 长度 B 票，不能仅回显 A 票。
- 检查 B 票的结构与签发时间。这是格式和时间校验，不声称能够验证上游签名。
- 两次探针均要求 HTTP 200，SSE 有完成事件，且没有错误或不完整事件。
- 第二发携带 B 新票；响应可以不返回票，或返回相同票。不同票意味着续接未通过。
- 第二发的 Cookie 值必须与第一发取得的配对一致；删除、过期、只更新半对或更换路由均不通过。
- A 票的来源有效性使用一小时规则；绝不会因为它已经生成 50 分钟，就用 B 的四分钟规则提前拒绝。

业务响应没有新票或回同一张票时，不续期。业务响应返回不同票或路由对改变时，撤销该代组合，
当前业务仍正常完成，下次业务请求重新获取。旧并发请求的迟到响应不会撤销已经更换的新一代组合。

## 5. A 的配置示例（v8）

把下面的内容合入 A 的现有配置。管理端口、密钥、来源凭据和出口沿用现有部署。
此示例为直连来源；A 已使用 Codex 模式时，保持既有 `runtime.enabled: true` 和 Runtime 地址，
并开启门票总开关即可，不需要增加接口参数或新的采集开关。

```yaml
config-version: 8
oauth:
  providers:
    codex:
      runtime:
        enabled: false
      force-websocket: false
      basispoints:
        enabled: false
      header-defaults:
        oailb-borrow: null
      turn-state-ticket:
        enabled: true
        cache-all-models: true
        refresh-before-seconds: 600
        probe-interval-seconds: 60
        models:
          - gpt-6-astra
          - gpt-5.6-sol
```

来源凭据 ID 使用 A 的凭据列表返回的稳定 ID。文件名和邮箱只用于识别，不作为取票主键。
如果 B 要请求的模型在 A 尚无快照，接口会返回不可借；不会偷偷用其他模型的票，也不会扩大 A 的主动探测列表。

## 6. B 的配置示例（v8）

```yaml
config-version: 8
oauth:
  providers:
    codex:
      runtime:
        enabled: false
      force-websocket: false
      basispoints:
        enabled: false
      header-defaults:
        oailb-borrow:
          source-url: "http://127.0.0.1:8318"
          source-auth-id: "A的稳定凭据ID"
          source-management-key: "A的管理密钥"
          source-instance-id: "A实例"
          source-auth-file: "codex-source.json"
      turn-state-ticket:
        enabled: true
```

- `source-url` 是 A 的实例地址，可以包含反向代理路径前缀；不要填写具体取票接口。
- `source-auth-id` 和 `source-management-key` 必填。
- `source-instance-id`、`source-auth-file` 是可选展示元数据，不参与来源账号选择。
- 本分支使用 v8 配置路径；旧分支的 `codex-header-defaults.oailb-borrow` 拼写仍可读取。
- CPAMP 原专用接口提交 `{}` 或 `null` 关闭借票；通用 v8 配置树仍使用 `null` 或删除字段。两者操作同一份设置。
- B 的普通 `models` 探测列表不限制借票模型；按本次请求最终 payload 中的实际模型向 A 取快照。
- B 的实验失败策略固定为“无有效票就报错”，不受普通 `fail-closed: false` 的放行策略影响。
- B 的已验证缓存仅在内存中；重启后由第一个业务请求重新获取和验证，不启动后台借票。

保存或加载明文来源管理密钥时，会将它加密成 `enc:v1:...` 写回配置。
加密方式兼容原 `oailb-borrow` 分支，数据密钥为当前 `oauth.auth-dir` 下的 `.oailb-data-key`，权限 0600。
配置 GET 返回保存的密文。迁移旧分支的密文配置时保留对应数据密钥；不能拿另一个实例的数据密钥解密。

## 7. 与 CPAMP 对齐的原有接口

2026-10-15 经 CPAMP「规划 CPA 集群化管理改造」话题核对，当前前后台并未整体迁往 v8。
本功能恢复原 `oailb-borrow` 已约定的 v0 接口；CPA 内部仍使用当前配置模型，兼容旧 YAML 和 v8 YAML。
只更改版本前缀不能兼容，下面的响应壳和关闭行为也是契约的一部分。

### CPAMP 配置读写

```text
GET /v0/management/codex-oailb-borrow
PUT /v0/management/codex-oailb-borrow
```

GET 未配置来源时返回 HTTP 200：

```json
{"supported": true, "config": null}
```

`supported` 表示这个版本具备接口能力，与总开关是否开启、是否有票据、是否已配置来源无关。
A、B 都提供该接口。选择来源时，CPAMP 先检查 A 的能力，然后沿用 `/v0/management/auth-files`
读取来源凭据，Windows 与 macOS 按各自稳定 ID 选择。

PUT 接受原来的五个字段：

```json
{
  "source-instance-id": "source-a",
  "source-url": "http://127.0.0.1:8318",
  "source-management-key": "A的管理密钥",
  "source-auth-id": "A的稳定凭据ID",
  "source-auth-file": "codex-source-windows.json"
}
```

保存成功返回 HTTP 200 `{"status":"ok"}`。密钥加密落盘，配置热重载；GET 用同样的
`config` 字段回读来源实例、凭据 ID、文件名等设置，只允许回读密文，不返回明文密钥。
提交 `{}` 或 `null` 关闭借用，随后 GET 返回 `{"supported":true,"config":null}`。
非法配置或保存失败不得改变正在使用的设置。

通用 v8 配置路径 `/v8/management/config/oauth/providers/codex/header-defaults/oailb-borrow`
继续可用；在 v8 树中修改设置后，原 GET 接口会立即回读同一份值。专用 v0 接口不会把现有 v8 YAML 降级为旧布局。
CPAMP 继续使用原来的页面与配置选择流程，无需修改项目代码。

### CPA 实例之间实际借票

恢复原取票入口：

```text
POST /v0/management/codex/oailb/borrow
Authorization: Bearer <A的管理密钥>
Content-Type: application/json

{"auth_id":"A的稳定凭据ID","model":"gpt-6-astra"}
```

请求中的 `model` 是新完整快照协议必需字段，不能省略后随意选择其他模型。
接口沿用 `available`、`value`、`cflb` 壳，并增加完整票据字段。HTTP 200 的可用响应结构：

```json
{
  "available": true,
  "value": "对应的__oailb",
  "cflb": "对应的__cflb",
  "account_id": "A的稳定凭据ID",
  "model": "gpt-6-astra",
  "state": "780长度的来源票据",
  "length": 780,
  "captured_at": "2026-10-15T00:00:00Z",
  "expires_at": "2026-10-15T01:00:00Z",
  "remaining_seconds": 600,
  "route": {
    "__cflb": {"value": "对应的__cflb"},
    "__oailb": {"value": "对应的__oailb"}
  }
}
```

`captured_at` 和 `expires_at` 保存来源原始期限；`remaining_seconds` 取票据与 Cookie 的更早期限。
每枚 Cookie 可以包含 `expires_at`。所有材料来自同一份快照，顶层 `value/cflb` 必须与 `route` 一致。
无完整可用组合、凭据缺失、禁用或票据总开关关闭时，沿用 HTTP 200 `{"available":false}`，不触发上游采集。
缺少 `auth_id/model` 返回 400；鉴权继续使用原管理密钥和访问限制，响应带 `Cache-Control: no-store`。

A、B 均需本次修正版。旧版本只返回 Cookie 的响应不具备票据和模型信息，B 会明确拒绝，
不能因为路径相同或 `available:true` 就视作成功。此前新增的
`/v8/management/credentials/codex/ticket-borrow` 专用路由已移除，不再保留第二条取票路径。
CPAMP 只负责配置来源，不请求或接收实际票据和 Cookie。普通凭据列表和下载仍隐藏这些材料。

普通 A 探针继续使用原 `[TICKET-PROBE]` 格式。B 的取票和两次验证改用 `[TICKET-BORROW]`，
并沿用触发本轮获取的业务请求 ID。两次探针保留各自真实的 session/turn 前缀。
B 的典型两次探针分别可见 `780/780`、`780/0`；第二发回同票时也可能为 `780/780`。
日志同时显示值是否相同，长度本身不代替上述续接验证。票据和 Cookie 原值不会写入日志。

```text
[2026-10-15 16:00:00] [a1b2c3d4] [codex-b-windows.json] [7a120003] [8b230003] [info ] [TICKET-BORROW] EXCHANGE 200 | 1.420s | gpt-6-astra/- | 780/780 | POST "https://chatgpt.com/backend-api/codex/responses" | ticket=changed
[2026-10-15 16:00:02] [a1b2c3d4] [codex-b-windows.json] [7a120004] [8b230004] [info ] [TICKET-BORROW] VERIFY 200 | 1.100s | gpt-6-astra/- | 780/0 | POST "https://chatgpt.com/backend-api/codex/responses" | ticket=absent
```

本轮不新增前端显示。原凭据页面的普通一小时票据摘要不代表 B 的四分钟已验证缓存；
判断 B 实验是否走通应结合探针日志和业务请求结果。

## 8. 回归测试

使用本地模拟 A 接口和模拟 ChatGPT 传输，不使用真实账号探测：

- A 保留到期前十分钟主动续期；A 票生成 50 分钟仍能重复借出；同票重存、JSON 恢复不延长期限；一小时及 Cookie 更早到期时不可借。
- 快照使用本次请求实际发出的 Cookie，usage 等其他请求更新 Jar 不会污染保存的配对。
- B 空闲不借票；61 秒复用、60 秒续借、到期拒用旧票；续借失败且旧票未到期时按原期限使用。
- 并发请求只获取一次、验证两次；凭据与实际模型分别隔离。
- 回显种子、过期票、结构异常、流错误、不完整流、换票、换路由、半对 Cookie、Cookie 删除均不通过。
- 业务返回新票只撤销当前代；迟到响应不撤销新代；已开始的 SSE 不因到期被中断。
- 两条 SSE 执行路径同 turn 强制替换，实际模型取最终 payload，过滤字段不会被恢复，失败不裸发。
- v8 管理密钥加密保存、配置往返与关闭、来源导出不包含 OAuth token。

## 9. 本次本地验证结果（2026-10-14）

- 借票、来源快照、两条 SSE 执行路径、配置加密与管理接口相关回归测试及 `-race` 检查通过。
- A 默认提前十分钟续期已保留，原有对应调度测试通过。
- `go build -o test-output ./cmd/server` 通过，临时构建产物已删除。
- `go test ./...` 存在一项主线已有失败：
  `TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile/completed`，
  完成态测试凭据缺少邮箱，收到“completed Codex OAuth credential is missing email or access token”。
  在未修改的主线基线 `e50b3414` 上单独执行同一测试也失败；本轮没有改动该 OAuth 测试或功能。
- 排除该已复现的基线测试后，`go test ./... -skip '^TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile$'` 全部通过。
- 验证使用模拟来源接口和模拟上游，没有调用真实账号，也没有部署、推送或打标签。

## 10. 借票分支发布

- 发布分支：`codex/oailb-borrow`。
- 分支标签格式：`oailb-borrow/vYYYY.MM.DD-N`。
- 本次接口修正版标签：`oailb-borrow/v2026.10.15-1`。
- 独立镜像仓库：`ghcr.io/power12317/cliproxyapi-oailb-borrow`。
- 本次镜像标签：`v2026.10.15-1`；完整地址为
  `ghcr.io/power12317/cliproxyapi-oailb-borrow:v2026.10.15-1`。
- 推送 `codex/oailb-borrow` 分支或分支专用标签，均由 `docker-oailb-borrow` 工作流构建 `linux/amd64` 和 `linux/arm64`，再发布多架构镜像。
- 日常更新统一使用 `ghcr.io/power12317/cliproxyapi-oailb-borrow:latest`，无需每次修改 Compose 版本号。
- 分支构建同时保留 `sha-<完整提交 SHA>`，标签构建保留 `vYYYY.MM.DD-N`，用于定位版本和回退。
- 工作流仅写入借票专用镜像仓库，不更新正式镜像的 `latest`。
- 通用 Release 工作流明确排除 `oailb-borrow/**`，不会为实验标签运行正式版出包流程。
- 镜像构建使用主线同款 Dockerfile，并在构建前刷新模型目录。

镜像发布成功后，A、B 均使用：

```bash
docker pull ghcr.io/power12317/cliproxyapi-oailb-borrow:latest
```

分支提交和标签推送成功只代表已经触发构建；镜像是否可拉取以工作流发布结果为准。

## 11. 同步 main 的核对结果（2026-10-15）

本次通过 SSH 更新并核对远端引用：远端 `main` 为 `e50b3414`，其全部提交此前已包含在借票分支中。
本地 `main` 为 `78dfbd73`，仅比远端多一条项目开发规范提交，本次已将本地 `main` 合并到
`codex/oailb-borrow`，没有发生冲突，没有额外缺失的 main 功能代码需要搬入。

新增规范要求保留官方 Codex 对应入口及平台原有的程序名、产品名、客户端名和版本标识；
涉及运行时集成或打包时，应根据官方构建输出验证这些标识，不通过改名或改写版本输出模拟其他程序。
这些规范不改变本次借票的运行行为。

近期原生 runtime 的其他改动仍位于 `codex/native-model-runtime`，不属于此次指定的 main 合并范围。
借票实现、A 提前十分钟续期、B 的 240/60 秒规则及独立镜像工作流均保留。
本次同步不移动已发布的 `oailb-borrow/v2026.10.14-1` 标签，也不创建新发布标签。

## 12. 接口整改验收（2026-10-15）

本次按用户要求和 CPAMP 负责话题确认的契约恢复 v0 原路径；这是复用既有功能协议的整改，
没有要求 CPAMP 整体切换 API 版本，也没有把实际取票接口当成配置接口。

新增生产路由集成测试覆盖：鉴权、支持状态与开关解耦、未配置、来源凭据筛选、原五字段保存、
密钥加密、重载回读、`{}`/`null` 清空、非法输入回滚、旧/v8 YAML 兼容、v8 通用设置与旧接口的一致性。
测试让 B 的实际借票代码访问 A 的实际生产路由，再通过模拟上游完成两次验证；明确验证旧的
Cookie-only 响应、缺少 `available` 的裸票响应、顶层与快照 Cookie 不一致均不能被接受。
另安排 CPAMP 负责话题通过本机临时 CPA 实例独立验收原页面后端链路，全程不使用真实账号。


实际联调结果：CPAMP 负责话题已用其真实集群服务连接两台本机 CPA 生产路由实例完成验收，
确认能力识别、mac/Windows 分别列出、选择 Windows 稳定凭据 ID、保存、重新读取和 `{}` 关闭均通过。
来源密钥在 CPA 配置文件中加密保存，CPAMP 返回给浏览器的借票配置响应未泄露来源密钥、OAuth token 或 Cookie。
验收后临时实例已关闭；没有修改 CPAMP 产品代码。

CPA 相关回归与竞态检查、服务端构建通过。全量测试仍存在此前已在主线复现的
`TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile/completed` 基线失败；
本轮接口整改未改动该功能。修正版继续发布到借票分支独立镜像，不覆盖正式镜像。
A、B 均须升级到 `v2026.10.15-1`；不应继续使用缺少原配置接口的 `v2026.10.14-1`。


排除上述已复现的主线基线失败后，`go test ./... -skip '^TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile$'`
全部通过。CPAMP 联调使用其 `codex/oailb-borrow` 分支提交 `7aa30006`，验收测试为
`TestExternalCPAOriginalBorrowContract`，结果由 CPAMP 话题独立记录；其工作区保持干净。


## 13. 自动 latest 与主线重大修复（2026-10-15）

借票独立镜像提供 `latest`。推送 `codex/oailb-borrow` 即自动构建，不再要求每次先打 tag。
两种架构均构建成功后，当前分支最新提交才更新 `latest`；历史 tag 重建或较旧提交的慢任务不会把它回退。
发布使用单独的并发组串行协调，正式 `cliproxyapi:latest` 不受影响。

此分支的 `docker-compose.yml` 和 `.env.example` 默认镜像已改为借票专用 `latest`，
Compose 默认拉取发布镜像。已有部署只需改一次镜像地址：

```yaml
image: ghcr.io/power12317/cliproxyapi-oailb-borrow:latest
```

如果使用本仓库的 `CLI_PROXY_IMAGE` 环境变量，也可以只在 `.env` 中设定一次：

```dotenv
CLI_PROXY_IMAGE=ghcr.io/power12317/cliproxyapi-oailb-borrow:latest
```

以后在 Compose 目录执行以下命令即可拉取并重建服务，无需改版本号：

```bash
docker compose up -d --pull always --no-build cli-proxy-api
```

本次已合入远端 main 的 `2b1de245`：修复启用 Codex runtime 后凭据启用/禁用状态没有持久化、
容器重启又恢复旧状态的问题。管理状态正常写回，Codex worker 对 token 刷新的管理权保持不变，
并保留 worker 已写入的新 token。上游已有回归覆盖反复启用/禁用、重启恢复及新 token 保留。
借票接口与 A/B 的续期规则继续保留。


本次验证：主线新增的凭据持久化回归、借票回归及相关竞态检查通过；构建通过。
全量测试首轮遇到已知 runtime OAuth 基线失败及一次未改动 WebSocket 测试的通道重复关闭；
该 WebSocket 测试单独连续重跑十次通过，随后仅排除已知 OAuth 基线失败的全量复核通过。
工作流通过 actionlint；执行脚本验证分支/标签版本、双架构合并、过期构建不回退 latest 及拒绝其他分支。
`docker compose config` 确认默认镜像是借票专用 latest，拉取策略为 always。


## 2026-10-15：实例 8 的 Codex 模式作为来源

本节对应本次借票分支镜像的实现。推送 `codex/oailb-borrow` 将触发现有独立镜像工作流；
镜像是否已构建完成，应核对对应 Actions 和实际运行版本。

实例 8（A）可以同时开启 Codex 模式与门票总开关，不配置 oailb 借用来源；
实例 3（B）保持直连 SSE、门票开启，来源选实例 8 的指定凭据。A 与 B 均使用
包含本次 CPA 改动的借票分支版本。

### 复用范围

- 读取现有 `cpa/inference/upstream` 请求和响应通知。票据来自响应头，或原有
  v3 正文中的 `response.metadata` / `codex.response.metadata`。
- Cookie 按同次实际请求 Cookie 与响应 Set-Cookie 配对，复用已有配对函数、
  票据结构、recorder、内存缓存和原借票接口。
- 普通响应采集不额外请求上游。不发送 `captureTicketSnapshot`，不依赖
  `ticketSnapshots` 能力或 `cpa/inference/ticket` 通知。
- 保持协议 v3，未引入 v5，未修改任何 HTTP/IPC 接口结构或 CPAMP 配置。

A 启动或打开总开关时立即检查来源票；到期前十分钟继续按原有定时器续期。
Runtime 凭据的补采通过已有 `cpa/inference/start` 调用执行现有 ping/pong
探针，仍由 worker 使用自己的身份、出口和设备设置；CPA 不直连上游替它打票。
采到需要的有效来源后即可结束获取，不要求 A 再做 B 的两次验证。

A 的普通对话票据仍由 codex-server 管理；CPA 不向 Runtime 普通业务请求注入
一小时票，也不会把 Runtime Cookie 灌入 CPA 的普通 OAuth Jar。

### 来源状态与期限

- Runtime 凭据可在既有凭据页面看到门票状态，只有票及配套 Cookie 可借时显示就绪。
- A 来源票保留原始签发时间加一小时，Cookie 已知更早到期时以更早期限为准。
- 相同票据重复出现不会续命；请求的 Cookie 沿用已知的相同值期限；SSE 元数据
  后到达时不会重新计算响应 Max-Age。
- Windows/macOS、凭据和实际模型独立；新上游尝试会清空当前交换配对，避免重试混票。
- B 的 240 秒期限、提前 60 秒按业务触发续借和 30 秒失败节流保持原有实现。
  B 界面填入普通门票的 3600 秒，不会改变借票缓存的 240 秒。

### 服务端前提

真实联调使用 codex-server main `024ada83c8a2aa2966cf49db4f0e36dc1adfd36c`
对应的 v3 产物，官方身份为 `codex-app-server 0.162.1`。
必须使用现有 IPC 返回 Cookie 原值的服务端；旧版只返回 `[REDACTED]` 时无法组成
完整来源。本文不代表远端镜像已经更新，应以实际部署版本为准。

### 本地验证

1. 响应头及两种 SSE 元数据事件采集、响应更新与请求 Cookie 继承、删除、期限、
   并发凭据隔离、旧脱敏数据拒绝、模型范围及门票开关均有回归覆盖。
2. v3 探针验证实际 RPC 字段没有增加，payload 规则只在最终请求上应用一次；
   被规则过滤的字段不会在发送前恢复。Runtime 不使用 CPA 的探测代理覆盖 worker。
3. 真实 worker 发起四次普通模型请求（含流式、非流式、响应头票及元数据票），
   A 缓存成功，CPA 直连来源请求为 0；凭据页面原接口显示就绪，同进程重载凭据仍可从内存导出，磁盘不含票据。
4. B 经 A 的原管理路由读取一次，发两次验证探针、两次业务请求，后续复用成功。
   B 读来源没有额外触发 A 打票。
5. 把 A 票置于提前十分钟窗口，调用采集器使用的原有采集函数，真实 worker 完成一次续期；定时触发另由虚拟时钟测试覆盖。
   A 合计五次上游请求、五次原有 Runtime 推理调用，CPA 直连仍为 0。
6. 实际 IPC 监测确认没有采集参数或独立 ticket 通知。

端到端测试使用真实 codex-server 和本地模拟上游、合成凭据；未使用用户真实账号。

最终验证结果：相关包与真实 worker 联调的 `-race` 检查通过；`gofmt`、
`git diff --check` 和服务构建通过。整仓测试唯一失败是此前已在 main 复现的
`TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile/completed`，
错误为 `completed Codex OAuth credential is missing email or access token`。
仅排除该既有用例后，最终代码的 `go test ./...` 全部通过。本次没有修改该 OAuth 用例或登录逻辑。

## 2026-10-15：补全 B 的借票诊断日志

无需新增配置项，默认 `info` 级别就能看到借票流程。只调整本项目现有借票分支，
没有新增或修改 HTTP/IPC 接口及请求参数，没有改动 CPAMP、codex-server。
A 的一小时期限、提前十分钟续期，B 的 240 秒期限、提前 60 秒按业务请求续借，
两次验证条件与 30 秒失败节流均保持原样。

### 日志阶段

| 标记后的阶段 | 含义及记录内容 |
| --- | --- |
| `ACQUIRE` | 开始本轮借票：来源实例地址；失败时记录具体阶段、原因及是否保留未过期旧票，不输出借票目标凭据名称或原始 ID |
| `SOURCE` | B 请求现有来源接口：HTTP 状态、完整耗时、模型、来源快照票据长度及组合剩余秒数 |
| `EXCHANGE` | 第一发上游请求：携带 A 的票和 Cookie 换取 B 票 |
| `VERIFY` | 第二发上游请求：携带 B 票验证是否可继续使用 |
| `READY` | 两次验证通过，记录保存的票长及组合剩余秒数 |
| `REUSE` | 本次业务选用了已验证的本地组合，记录其剩余秒数；续借失败保留旧票时也明确记录 |
| `WAIT` | 等待并发请求正在进行的借票，或处于失败后的节流期；显示首次失败原因及获取请求 ID |
| `SEND` | 实际发送前发现所选票已过期或被撤销，明确记录本次业务尚未发往上游 |
| `REVOKE` | 业务响应改变票值、Cookie 值/配对或缩短期限，撤销当前组合；下次业务请求重新借票 |

这些记录统一带 `[TICKET-BORROW]`。`SOURCE`、`EXCHANGE`、`VERIFY` 和最终 `ACQUIRE/READY`
使用同一个业务请求 ID；后续业务有自己的 ID，`WAIT` 会引用首次失败的 ID。
前缀中的凭据文件是 B 本次使用的本机凭据；借票目标 A 的凭据名称及原始 ID 不写入日志，
`ACQUIRE` 和 `SOURCE` 均不再输出 `credential` 字段。
两次探针前缀的 session/turn 是实际探针自己的标识，不再为每条日志另造无关联的请求 ID。

请求行保留状态、秒数、模型、请求/响应票长、方法与地址。来源接口的 `0/780`
表示从响应 JSON 快照取到了长 780 的来源票；上游请求行的长度来自实际请求/响应头。
地址记录实例主机及路径，移除用户名、密码、查询串及片段。

`EXCHANGE/VERIFY` 的耗时覆盖收到响应头、读取 SSE 和检查完成事件、Cookie 的全过程。
HTTP 200 后读流失败时，仍显示真实 HTTP 200，同时使用 `warn` 并附上读流原因。
随后完成的票据结构、时间及同值验证如有失败，会在 `ACQUIRE` 中给出具体原因；
只有看到 `READY` 才表示整轮验证已通过。

### 同为 780 长度时如何区分

- `ticket=absent`：响应头没有票；`ticket=same`：响应票与发送票完全相同；
  `ticket=changed`：值不同，即使两边都长 780。
- 日志不输出 Cookie 长度，也不计算日志用的 Cookie 长度或逐枚比较统计。
- `READY/REUSE` 的 `remaining` 取票据与两枚 Cookie 的较早期限。
- 原始票据、Cookie、Authorization、管理密钥及来源 JSON 正文不写入日志。
  HTTP/SSE 错误会提取 `type`、`code`、状态码和 `message`，兼容顶层及嵌套错误对象；
  输出前移除已知凭据、票据、Cookie 等敏感值，不输出完整响应对象。

以下是“第二次响应仍长 780，但实际返回另一张票”的示意，**不是对用户旧日志原因的认定**：

```text
[2026-10-15 16:00:00] [a1b2c3d4] [codex-b.json] [session1] [turn0001] [info ] [TICKET-BORROW] SOURCE 200 | 0.010s | gpt-6-astra/- | 0/780 | POST "http://instance8:8317/v0/management/codex/oailb/borrow" | remaining=599s
[2026-10-15 16:00:02] [a1b2c3d4] [codex-b.json] [probe001] [probe002] [info ] [TICKET-BORROW] EXCHANGE 200 | 1.420s | gpt-6-astra/- | 780/780 | POST "https://chatgpt.com/backend-api/codex/responses" | ticket=changed
[2026-10-15 16:00:03] [a1b2c3d4] [codex-b.json] [probe003] [probe004] [info ] [TICKET-BORROW] VERIFY 200 | 1.100s | gpt-6-astra/- | 780/780 | POST "https://chatgpt.com/backend-api/codex/responses" | ticket=changed
[2026-10-15 16:00:03] [a1b2c3d4] [codex-b.json] [session1] [turn0001] [warn ] [TICKET-BORROW] ACQUIRE | gpt-6-astra | verify: continuation returned a different ticket (780/780); retry in 30 seconds; business request not sent
[2026-10-15 16:00:04] [b1b2c3d4] [codex-b.json] [session1] [turn0001] [warn ] [TICKET-BORROW] WAIT | gpt-6-astra | acquisition retry is deferred for 29 seconds; last failure: verify: continuation returned a different ticket (780/780); acquisition request: a1b2c3d4; business request not sent
```

后续 503 的 `error.message` 同样保留 `last failure` 和首次请求 ID，并显示当前剩余等待秒数，
不再一直只显示固定“等待 30 秒”。既有 `RetryAfter()` 策略仍为 30 秒，本次不修改调用方重试策略。
节流期间不会因此多取一次来源票或多发探针。后续成功会清除旧失败原因。

### 本次用户日志能够确定的范围

用户日志显示运行的是 `fb068f77`。15:49:20、15:49:33 的两条旧 `TICKET-PROBE 780/780`
来自 B 借票探针；能够携带来源票进入探针，说明来源取票与来源组合校验已经通过。
但旧日志在收到响应头后立即打印，没有完整响应耗时、票值比较及验证失败原因，
不能据此判断到底是第二张票变化、Cookie 变化还是后续响应内容失败。

15:49:43 的首次业务 503 耗时 26.343 秒，随后多条短耗时 503 是节流期间的拒绝。
这些业务行的 `0/0` 不表示一次成功的空票上游请求：获取组合失败时，业务尚未发出。
本次补齐可观测性，供部署后复测定位具体原因，不宣称已修复尚未确定的上游验证失败原因。

### 新增回归覆盖

- 来源取票、两次验证和最终结果与业务请求 ID 对应；探针保留真实 session/turn。
- 缓存复用、续借失败保留旧票、业务撤销均有日志，且网络请求次数保持不变。
- 两张同长不同值的票被拒绝，首次原因在后续节流返回和日志中保留。
- 用虚拟时钟模拟 HTTP 200 后读流超时，验证日志包含完整耗时及失败原因。
- 来源 HTTP 503、回显来源票、票结构错误、SSE 未完成/限流、单枚 Cookie 更新、
  第二次 Cookie 改变及传输错误都有可识别原因。
- 成功后清除旧原因；剩余等待时间递减；普通 A 探针的原始格式保持不变；日志无原始凭据材料。

本次验证：上述新增测试、原借票测试和普通探针格式测试通过；日志、执行辅助、SSE 执行器及
API 相关回归的 `-race` 检查通过，其中包括真实 v3 worker 的来源采集到 B 借票联调。
`gofmt -w .`、`git diff --check`、`go build -o test-output ./cmd/server` 通过。
排除前文已确认的主线 OAuth 基线用例后，`go test ./...` 全部通过。

## 2026-10-15：门票改为仅保存在内存

本次不增加配置开关，直接将门票存储改为内存模式：

- A 主动探测、普通响应以及 Codex Runtime 现有响应中取得的门票和对应的
  `__cflb`、`__oailb` 快照，一并存入独立并发安全缓存，不再放进 `auth.Metadata`。
- 采集门票不再调用凭据管理器的保存方法，不会因为门票更新而写 auth JSON 或触发文件 WRITE 重载。
  凭据自身的 OAuth 更新、启停等正常保存行为保留。
- 凭据页面的门票状态、普通请求强制注入以及原有跨实例供票接口都读取同一份内存缓存。
  CPAMP 配置、页面和接口无需调整。
- 不同凭据、macOS/Windows 身份、不同模型继续隔离；同邮箱不合并门票。
  同一身份的 token 刷新及普通文件/配置重载保留内存票。删除凭据或切换身份时清理旧缓存。
- 缓存保存和读取都会复制 Cookie 快照，避免后续 Jar 更新或调用方修改污染已有票据配对。
  并发晚到的旧票不会覆盖较新的正常响应票；重复观察相同票不延长原到期时间。
- A 的一小时有效期、启动/开启时检查采集、提前十分钟主动续期均保持不变。
  B 的票本来就是内存缓存，继续按 240 秒有效、剩余 60 秒时由业务请求触发续借。
- 进程重启后 A、B 缓存均为空。A 开启门票时按原流程重新采集；B 仍等业务请求到来再借票。
  A 的非探测模型等待正常响应再次采集，不会因此加入主动探测列表。

旧版 auth JSON 中的 `codex_turn_ticket:*` 不再加载或用于兜底。升级时不会仅为清理这些字段
批量改写凭据；后续正常保存凭据时会剔除旧门票字段，包括 Codex 共享凭据合并和 token 刷新写入路径。
因此旧文件可能暂时仍能看到历史字段，但运行中的门票不会读取它们，也不会再向文件保存新门票。

回归覆盖主动探测与正常采集不写文件、额外模型的状态与注入、同进程重载、清空缓存后
不恢复旧 JSON、凭据和模型隔离、并发新旧票顺序、Cookie 快照复制及旧文件字段的安全迁移。

验证结果：上述回归与相关包的 `-race` 检查通过；真实 v3 worker 的正常响应采集、
原有来源接口供票、B 两次验证及复用、A 续期通过，确认不调用凭据保存且磁盘无新票据字段。
`gofmt -w .`、`git diff --check` 和服务构建通过；仅排除前文已有主线 OAuth 基线失败用例后，
最终代码的整仓测试全部通过。

## 2026-10-15：目标凭据名称移除与 16:42～16:48 日志分析

### 目标凭据名称

删除借票开始和来源响应日志中的 `credential=...`，不输出借票目标 A 的文件名、邮箱或原始凭据 ID，
也不将这些值放进日志的结构化字段。来源配置和实际请求中的凭据选择仍按原方式工作。
原请求日志前缀中的本机执行凭据 B 沿用原格式。回归测试同时检查日志正文、格式化结果和结构化字段。

### 本地 auth JSON 的 WRITE

门票缓存与凭据保存是两条路径。A 门票/配套 Cookie 缓存、B 的四分钟借票缓存都不调用文件保存，
也不进入 `auth.Metadata`。但通用请求结果处理 `Manager.MarkResult` 仍会走
`persistLocked → persist → Store.Save`，成功请求和请求级 503 都可能触发这条路径。
此外，OAuth 刷新、管理操作和旧字段清理仍可正常保存凭据。

`FileTokenStore` 对普通 JSON 内容相同的保存会跳过写入；共享凭据写入和具体 TokenStorage 的行为
还取决于使用的保存分支。升级后旧 `codex_turn_ticket:*` 被剔除，也可能使首次普通保存改变文件内容。
文件 watcher 的 `WRITE` 行只说明检测到文件变更，没有记录具体写入调用栈或字段差异。
因此该行不能证明门票仍然写盘，也不能单凭时间顺序断言每一次都是 OAuth 刷新。

本次补充生产保存路径回归：先在内存保存票，再报告业务成功和请求级 503；确认两次结果处理都调用
通用凭据保存，但不会写入内存票；旧门票字段在正常保存时被清除，内存票仍可复用。
本次没有停止正常凭据保存，也没有改动请求结果统计或凭据冷却规则。

### 日志中的实际借票结果

以下时间沿用用户日志原文，不做时区换算；不会在文档中复制真实凭据名称。

| 时间/请求 ID | 事实 | 结论 |
| --- | --- | --- |
| 16:42:18～16:42:24，`e2af66fe` | `EXCHANGE 780/780 changed`，随后 `VERIFY 780/0`，最后 `READY remaining=221s` | 第一轮换票与验证成功 |
| 16:42:35、16:42:57 | 两次业务请求均为 HTTP 200、`780/0` | 新 B 票确实成功用于业务，后续复用也有效 |
| 16:42:58，`801c4f64` | 使用缓存，剩余 187 秒；16:48:22 才结束，HTTP 200、`780/0` | 发出时票未过期；已经开始的长 SSE 没有因本地票过期而被中断 |
| 16:46:53，`88c0a910`，`gpt-6-luna` | `source ticket bundle is unavailable` | A 对这个实际模型未提供可借完整组合；没有执行上游换票 |
| 16:46:53～16:46:59，`26b1a7d5`，`gpt-6-astra` | 第一发换票成功；第二发 `VERIFY 780/780 changed` 且 SSE `type=error` | 第二轮得到的新 B 票没有通过续接验证，业务尚未发送 |
| 16:47:31、16:48:23、16:48:55 开始的三轮 | 第一发 `EXCHANGE` 就返回 SSE `type=error` | 后续失败发生得更早；不能将所有 503 都归结为第二次验证 |

第一轮的三个剩余时间都指向约 **16:46:05** 的本地组合期限。16:46:53 再次借票时旧组合已经过期，
重新取票符合既定规则，不是 CPA 持续把过期 B 票发给上游。重复 `WAIT` 行只执行本地失败节流，
不会每条都触发一次来源取票或上游换票。

对 `gpt-6-luna`，确定的是来源接口返回“无可用组合”。未采集这个模型、票/Cookie 到期或快照不完整
都可能产生同一结果；不能把它解释为已有 B 票失效，也不能拿 Astra 的票在本地跨模型替代。

对 Astra，确定的是第二轮新票的验证请求失败，之后换票请求也失败。日志不足以证明
“同一张 A 票只能借一次”：没有记录各轮 A 票是否相同，Cookie 相同长度也不等于值相同；
而 `type=error` 本身不是“票据无效”的专用错误码。第二轮探针还与上一条长 SSE 重叠，
账号并发限制、上游错误或借用路由限制都不能仅凭这些行排除。

本次同时补齐了已有错误码提取的一个缺口：`{"type":"error","code":"rate_limit_exceeded"}`
这类顶层错误码此前没有输出，现在会保留 `code` 及嵌套 `error.type`；测试版同时补充经过敏感值清理的 `message`。
已有日志没有原始 SSE 错误对象，不能回溯恢复被省略的码，因此本次不声称已确定具体上游拒绝原因，
也不擅自修改一小时 A 票、240 秒 B 票、续期或验证策略。

本次验证：目标名称/原始 ID 不出现在日志正文或结构化字段、顶层错误码提取、请求结果保存与
内存票隔离的回归及 `-race` 检查通过；相关借票、SSE、门票、普通探针格式和文件迁移测试通过。
`gofmt -w .`、`git diff --check` 和服务构建通过。


## 2026-10-15：精简 Cookie 日志并发布诊断测试版

- 删除 `SOURCE`、`EXCHANGE`、`VERIFY`、`READY`、`REUSE` 等日志中的 Cookie 长度字段，
  同时移除相应长度计算与仅供日志展示的逐枚 Cookie 比较循环。
- 保留票据的请求/响应长度、票值是否相同、剩余时间、请求阶段与接口地址。
- 上游 HTTP 非 200 时读取有界错误响应，输出实际 HTTP 状态、JSON 错误码及错误说明。
- SSE 的 `type=error`、`response.failed` 等错误同时提取顶层及嵌套的错误码、类型、状态和说明。
  不再因为错误码不在预设名单中，就只留下 `type=error`。
- 原始 `message` 先清理本轮已知的目标凭据、管理密钥、OAuth token、票据和 Cookie；
  再转义换行并限制长度。只提取错误相关字段，不打印完整响应、来源快照或请求正文。
- 这些错误详情进入同一轮日志、首次 503 和后续等待期 503，继续保留请求 ID 和失败阶段。
- 上一轮的借票目标名称移除一同发布。接口地址、参数、借票验证和时间规则不变。

以下仅演示输出结构，不代表已经确认真实上游返回了这个错误：

```text
[TICKET-BORROW] VERIFY 200 | 1.100s | gpt-6-astra/- | 780/780 | POST "https://chatgpt.com/backend-api/codex/responses" | ticket=changed | probe stream reported failure: type=error, code=concurrent_request_limit_exceeded, status=429, message="Too many concurrent requests."
```

日志不再包含 Cookie 长度；如果 Cookie 配对本身导致验证失败，仍会输出具体失败原因。
测试版沿用 `ghcr.io/power12317/cliproxyapi-oailb-borrow:latest`，同时由构建流程提供对应提交的 `sha-…` 镜像。

验证：HTTP/SSE 错误码与说明能够到达日志、首次失败及等待期错误响应；目标凭据及实际票据/Cookie
不会出现在错误内容中；Cookie 长度字段与计算代码已删除。相关回归、`-race`、真实 v3 worker 联调、
格式检查和服务构建通过。整仓回归仅排除此前已确认的 OAuth 基线失败用例后通过。

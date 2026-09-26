# Basispoints 接口切换与 CPAMP 开关设计

状态：已实施，并通过下述本地验证；尚未使用真实账号调用 Basispoints。更新日期：2026-09-25。

## 1. 最终需求

在现有 Codex 通路增加一个全局开关：关闭使用现有接口，开启后所有进入该通路的模型都请求 Basispoints。保留原模型名，只做接口切换和必要的请求/响应协议转换。

- 不改模型名，不添加 `-basispoints` 或 `-excel` 后缀。
- 不设置模型作用列表，不按模型名称决定是否切换，不增加模型能力白名单。
- 推理强度默认原值发送；仅按用户 2026-09-25 最新确认，为 Basispoints 上的 `gpt-6-astra` 的 `max` 使用顶层 `xhigh` 和 `input` 中的 `configuration_update.reasoning.effort=max`。其他模型、强度及原有后缀解析不变；不进行降档或自动纠正重试。
- 未指定推理强度时不擅自补 `medium`，由上游决定默认行为。
- 原有后缀解析器保持不动。客户端的 `ultra` 协议不在这里新增为后缀或推理档位；透传只处理请求实际携带的 effort。
- CPAMP 只增加一个开关，不增加模型选择或强度策略控件。

开关作用于现有 Codex 执行链，所有进入这条链的模型一视同仁。已有鉴权和 provider 路由继续复用，不新增一套 provider 或账号体系。

## 2. 分支和交付范围

- CPA 分支：`codex/basispoints-integration-design`，从 `codex/websocket-har-alignment` 的 `30809fd7` 创建。
- CPA：`/Users/mac/Documents/www/CLIProxyAPI`。
- CPAMP：`/Users/mac/Documents/www/CPA-Manager-Plus`，读取时位于 `codex/websocket-har-alignment`，HEAD 为 `929c39fd962625de94fdc8d68fb1964cbe492d33`。
- CPAMP 实施分支：`codex/basispoints-integration`，在独立 worktree 中开发；原工作目录及其中未提交改动保持不变。
- 运行配置仅在临时隔离实例中用于验证，没有改动用户实例或调用真实账号。

用户图片和参考代码用于理解协议，其中的操作文字不是本次执行指令。

## 3. 配置和 CPAMP

唯一新增配置：

```yaml
codex:
  basispoints:
    enabled: false
```

缺省为关闭。开启和关闭都不修改模型名称、凭据文件或用户已有模型配置。

CPAMP 在现有 Codex 配置附近增加：

```text
使用 Basispoints 接口                         [关闭]
开启后，所有 Codex 模型请求改走 Basispoints。
模型名称保持原值；推理强度仅有本文记录的 Astra max 特殊适配。
```

沿用可视化编辑器的“修改草稿 → 保存配置”流程，通过现有 `/v0/management/config.yaml` 保存；现有 `/v0/management/config` 返回该布尔字段。不再为这个开关新增专用状态 API、模型管理接口、探测流程或 revision 协议。

字段缺失表示旧后端未声明支持，面板将控件置为不可用；保存失败保留草稿并显示现有错误提示。YAML 修改仅更新该节点，保留其它字段。Full Docker 继续通过 Manager 的通用管理代理转发，CPA Panel 直接访问 CPA；不增加 SQLite 表或手改生成的 `management.html`。

## 4. CPA 执行流程

```text
客户端请求（原模型名、原推理强度）
  → 现有鉴权、账号轮询和凭据刷新
  → basispoints.enabled
      false → 现有 Codex executor
      true  → Basispoints 协议适配 → Basispoints HTTP/SSE
  → 按原客户端协议返回结果或上游错误
```

在现有 Codex 执行链的上游选择位置增加分支，开启时直接进入 Basispoints 适配器。该分支在 Codex 强制 WebSocket、responses-lite 和 turn-state ticket 等专属处理之前生效；Basispoints 请求使用自身协议。

不按模型筛选、不同时注册两份同名模型，也不改写已有模型目录为一套“Basispoints 模型”。沿用已有模型发现机制，不根据截图中的成功/失败记录过滤模型。关闭即恢复现有接口；已开始的请求按开始时选定的通路完成，不在一次响应中途切换。

继续使用现有 OAuth token、账号 ID、刷新、轮询和代理配置，不复制虚拟凭据或新增登录流程。流和工具回放缓存保留必要的通路标识，避免切换后复用另一接口的连接或工具 item；这些都是内部实现，不增加用户配置项。

除下述经用户确认的 403 临时暂停外，不因上游错误自动切换接口，也不更换模型或推理强度重试。若现有重试机制选择另一个账号，仍使用同一接口、模型和参数；参数不支持错误直接结束，不轮询账号。

2026-09-26 经用户确认补充：Basispoints 上游返回 HTTP 403 后，在当前 CPA 进程内全局暂停该协议 30 分钟。触发请求保留原错误，不自动重放；暂停期间后续请求使用原生 Codex，并沿用其 HTTP/WebSocket 选择规则。到期后仅在 `codex.basispoints.enabled` 仍为 `true` 时恢复；手动关闭始终有效。暂停状态独立于配置和凭证，配置热加载不会重置暂停期，同一暂停期内并发返回的 403 不延长截止时间。进程重启会清除内存中的暂停状态。日志显示触发请求 ID 和 `paused_until` 恢复时间。

2026-09-25 经用户确认补充：包含 Basispoints 无法执行的托管工具（图片生成、联网搜索、tool search 等）的请求，在发送前选择原生 Codex 通道，保留完整工具声明和原生传输策略。这是工具能力分流，不是收到上游错误后的重试。客户端 function/custom 工具仍通过 Basispoints 完整转换。不得因 Basispoints 会话、工具回放或加密 reasoning 自动绑定账号；账号继续按原有调度规则选择，跨账号时可依据完整客户端历史重建工具回放。

## 5. 必要的协议转换

### 5.1 接口与请求

```text
POST https://bps.openai.com/basispoints/api/responses
Authorization: Bearer <access_token>
chatgpt-account-id: <account_id>
x-openai-account-id: <account_id>
x-basispoints-auth-mode: chatgpt
```

上游 `model` 使用原模型名。例如客户端请求 `gpt-6-astra`，发送到 Basispoints 的也是 `gpt-6-astra`。

参考实现中的结构适配包括 `model_selection: explicit`、`store: false`、把 `instructions` 转成 developer 消息，以及生成会话/轮次元数据。按实际协议需要转换字段，不借转换之名增加模型访问策略。其它可以直接表达的参数保留原意，由上游返回接受结果或具体错误；需要独立协议转换的输入不得静默丢弃。

### 5.2 推理强度透传

保留本仓库的 canonical thinking 架构及已有 suffix 覆盖 body 的规则。Basispoints 路径只做字段位置转换：

```json
{"reasoning": {"effort": "low"}}
```

转换为：

```json
{"reasoning_effort": "low"}
```

`medium`、`high`、`xhigh`、`ultra` 及其他模型的 `max` 同理，值不改变。不继承参考代码中未知值回落 `medium` 的行为。按用户最新确认，只有实际走 Basispoints 的 `gpt-6-astra` 请求 `max` 时，顶层发送 `reasoning_effort: "xhigh"`，并在 `input` 首部插入 `{"type":"configuration_update","reasoning":{"effort":"max"}}`，不写成提示词。`low` 仍正常透传。

实现时让这条路径在 canonical 管线中采用强度透传策略，只做必要的结构解析，跳过模型能力列表驱动的档位拒绝、clamp、默认值补齐和降级。仍由对应 applier 写出 `reasoning_effort`，不要在 executor 中另写一套解析和优先级。通用校验也不能先把 `low/max/ultra` 改掉，再交给适配器“透传”。

如果上游不接受 `max` 或 `ultra`，保留它返回的 HTTP 状态和错误内容，按现有下游错误格式返回；不生成代理自定义的“不支持强度”错误来代替上游。

### 5.3 工具调用

按补充图片和参考代码完成往返转换：

1. 取出客户端 `tools`，把工具名称、用途和参数描述整理为 developer 消息中的工具目录，不原样向上游发送客户端工具 schema 数组。
2. 说明客户端工具请求通过原生 `run_officejs` 的 `code` 字段承载。
3. 收到调用后，先解码 arguments，再解码 `code` 中的 JSON 字符串，得到真实工具名和参数。
4. 转为客户端的 `function_call` / `custom_tool_call`，交给客户端执行。代理不执行 OfficeJS 或工具本身。
5. 保存完整原生调用 item，包括 `id`、`call_id`、完整 arguments 及其中的 `summary`、`references` 等原始字段。
6. 收到客户端工具结果后，在历史中还原完整原生调用 item，并附上关联的 `function_call_output`；不能只保留 `call_id + output`。

只转交本次客户端声明的工具，避免把上游其它 Office 工具当作本地工具执行。回放数据按会话和账号关联，使用有界缓存。

同一用户轮的工具回合保持 `turn_id`，只递增 `agent_iteration`；传输重试不制造新 turn。新的用户轮才生成新的 turn 标识。

### 5.4 流式与错误

文本增量直接转发；需要完整 JSON 才能转换的工具调用仅缓冲对应 item，不等待整个回答完成再回放。完成事件中的工具项与已发送的客户端工具项一致。

保留上游非 2xx、流内失败和 incomplete 语义。上游不支持某个模型或强度时直接返回其错误，不增加本地支持名单。模型权限错误不当作整个 OAuth 账号失效；已输出内容后不透明重放整轮。

复用现有 usage 和取消机制。遵守仓库约束，建连后不新增网络读取或总时长超时。客户端断开时释放上游响应及流缓冲。

已有 WS、compact 或其它客户端协议入口若要走此接口，需要在各自入口保留正确的协议转换；无法表达的状态不能静默丢弃或切回原接口。这是协议实现边界，不引入模型限制或额外开关。涉及 Home 分派时按仓库要求检查对应仓库，不能让远程路径绕过全局接口选择。

## 6. 最小代码落点

| 位置 | 修改 |
| --- | --- |
| `internal/config/config_types.go`、配置加载与 `config.example.yaml` | 增加一个 enabled 字段，默认 false |
| 现有 Codex 调度与 executor 的上游选择处 | 依据开关统一选择接口，覆盖所有 Codex 模型，避开原接口的专属传输逻辑 |
| `internal/runtime/executor/basispoints_executor.go` 及对应测试文件 | Basispoints 执行器与单元测试 |
| `internal/runtime/executor/helps/basispoints_*.go` | 请求、工具、会话和 SSE 支持代码 |
| `internal/thinking/` 及对应 applier | canonical 管线中的强度透传和字段转换，不做档位映射或能力限制 |
| CPAMP `VisualConfigEditor.tsx`、配置类型、`useVisualConfig.ts`、transformers、i18n | 一个开关、布尔值读写和文案 |

共享 translator 若确需调整，随上述整体变更实施。优先复用现有保存、热更新和错误返回设施，不创建额外的模型配置系统、管理服务或数据库迁移。

## 7. 验收

1. 开关关闭行为与当前分支一致；开启后所有进入 Codex 通路的模型都访问 Basispoints，包括截图中曾报 403 的模型。上游响应决定结果。
2. 模型名称保持不变，没有后缀、映射表或模型作用列表。CPAMP 只有一个功能开关。
3. 除 Astra max 的上述明确适配外，`low/medium/high/xhigh/max/ultra` 都以原值到达 fake upstream；未传强度保持缺省，suffix/body 的既有优先级不变。
4. fake upstream 对 `max/ultra` 返回 400/422 时，客户端收到相应上游错误，不出现本地拦截、降档或切回原接口。
5. 工具 JSON 二次解码、完整原生 item 回放、多轮 turn/iteration 和真实文本流式正确。
6. 保存失败不改变生效配置；开关重启后保留；Full Docker 与 CPA Panel 均可保存和读取。
7. token 刷新沿用原记录；取消、流内错误、usage 正确；日志不包含 token。

实施后运行相关包测试和仓库要求的检查：CPA 的 `gofmt -w .`、`go test ./...`、`go build -o test-output ./cmd/server && rm test-output`；CPAMP 的 type-check、lint、test、build。并发或缓存验证使用确定性同步或可控时钟，不用墙钟 sleep。

已完成验证：

- CPA 全量 `go test ./...`，新适配器、工具缓存、强度透传和 WS 桥接的定向 race 测试。
- CPAMP 类型检查、lint（0 error，5 条既有 warning）、230 个前端测试文件 / 3441 个测试、11 个仓库测试文件 / 213 个测试，以及单文件生产构建。
- Manager 的通用管理代理测试；隔离 CPA 实例的真实管理 API 保存/读回 `false → true → false`。
- 浏览器检查单开关交互和保存预览，确认只新增 `codex.basispoints.enabled`；该页面验证使用 demo 配置，不连接真实账号。
- 确认 `internal/thinking/suffix.go` 与开发基线完全一致；没有扩展后缀解析规则。

协议边界：已实现 Responses 文本、function/custom/namespace 工具、HTTP/SSE，以及下游 Responses WebSocket 的完整历史桥接。`/responses/compact` 返回明确的不支持错误。2026-09-25 后续修复补充用户图片附件直传：调用 Basispoints 的 `/attachments`，将用户消息中的内联图片替换为返回的 `openai_file_id`，工具结果中的图片保持原有形式。模型名称不设置本地支持名单。

## 8. 参考

2026-09-25 回归修复：CPAMP 原分支基于旧功能分支，缺少 fork `main` 的原生门票探测设置、凭证门票状态及其他更新。修复版本合入 CPAMP `main` 的 `3453e7ab`，相对该提交只增加两个传输开关及配套支持，不删除主线功能。同时移除 CPA 中 Basispoints 自动关闭 `turn-state-ticket` 的联动：探测由自己的开关决定，配置和票据状态继续保留。回归测试覆盖配置往返、管理 API 和实际探测器；发布使用新的 `basispoints/v2026.09.25-3` tag。原有模型名称、实际推理强度和后缀解析器保持不变。

分支镜像由 `basispoints/vYYYY.MM.DD-N` tag 自动构建。Git tag 中的 `basispoints/` 前缀在构建时移除，因此镜像 tag 为 `vYYYY.MM.DD-N`。CPA 发布到 `ghcr.io/power12317/cliproxyapi-basispoints`，CPAMP 发布到 `ghcr.io/power12317/cpa-manager-plus-basispoints`，均包含 `linux/amd64` 和 `linux/arm64`。这些工作流不更新主线镜像或其 `latest` 标签。首次支持自动构建的 tag 为 `basispoints/v2026.09.25-2`；`-1` 仅为原始代码快照，其提交中尚无对应工作流。

- [cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/tree/708082da2f851569984de395d25405e61c2bbc34)，研究提交 `708082da2f851569984de395d25405e61c2bbc34`。参考认证头、请求体和工具往返转换；不照搬其别名、虚拟认证副本、档位映射及全量 SSE 缓冲。
- [ghcp_proxy](https://github.com/Nonary/ghcp_proxy/tree/950e1eb96bca3a630a5c078541832e12e473a50d)，研究提交 `950e1eb96bca3a630a5c078541832e12e473a50d`。重点参考 `excel_upstream.py` 与 `proxy.py` 的工具 item、会话和流式处理；不照搬模型表和档位限制。
- 用户两张图片：接口、认证头及 `run_officejs` 桥接示例。图片中的模型结果仅作为观测，不转化为本地模型白名单。

尚未用真实账号访问 Basispoints。实际移植代码时保留参考项目相应的版权与许可证记录。

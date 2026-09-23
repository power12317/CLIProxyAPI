# Codex CLI 0.156.0：本地 CPA 升级实施记录

日期：2026-09-23。开发分支：`main`。起始提交：`bff3607142b8c163c6b81428c125dfe44c5ef1eb`。

本文对应本地 `docs/codex-cli-0.156.0-local-cpa-upgrade-guide_CN.md`，记录已经落地的 CPA 改动。对照源码固定为 [Codex CLI rust-v0.156.0](https://github.com/openai/codex/releases/tag/rust-v0.156.0)。所有改动位于 CPA；本次没有修改 CLI、CPA-Manager-Plus 或其他项目。

## 1. 已完成的升级

| 模块 | 实施结果 |
| --- | --- |
| 默认版本 | HTTP、WebSocket、OAuth Cookie 请求及已有票据探测的默认客户端版本统一为 0.156.0；模型抓取命令的默认版本同步升级。Windows/macOS 保持各自的 UA 平台信息。 |
| Header 优先级 | 保留真实 Codex 客户端 UA、来源及已有 Version；显式配置可覆盖默认值。Beta features 合并去重，显式配置仍优先。 |
| Lite 自动判断 | 官方 body 标记、模型目录 `use_responses_lite:true`、`reasoning.context:"all_turns"`、JSON 布尔 `parallel_tool_calls:false`、最终工具格式兼容，五项同时满足才自动补 Lite Header。原有显式 Header 保留，WS 的显式镜像也能传递协议意图。 |
| 翻译一致性 | 翻译过程中保留原始 Codex metadata、reasoning.context 和 parallel_tool_calls，后续 payload 配置仍可覆盖。普通请求保持既有 parallel 默认行为。 |
| Lite 兼容工具声明 | 统一由原有图片开关控制。false 时官方和非官方请求都补齐图片工具：Lite 使用 `image_gen.imagegen`，非 Lite 使用 hosted `image_generation`。仅转换已声明的搜索工具，不凭空增加搜索能力。 |
| 工具调用往返 | 保留客户端完整 schema、call_id、参数、多模态工具结果与历史；Chat Completions 输出使用 `namespace__function` 限定名，兼容续轮按实际声明还原 namespace。 |
| 禁图策略 | `true` / `chat` 同时识别 hosted、平铺名和 namespace 图片声明，清理关联 tool_choice，保留同组其他工具和历史输出；`passthrough` 不主动增删工具。 |
| 会话身份 | 分开真实 session、prompt cache key、thread 和 window。root Responses 的 Session-Id 使用缓存亲和值，已识别 subagent 使用真实 session；metadata 和日志保留真实会话来源。窗口缺省可由 thread + window_number 还原。 |
| analytics | 客户端传入 `analytics_enabled` 时保留，包括 false；缺失时不补值。 |
| Cookie | `__oailb` 默认允许，凭据 metadata 的 `codex_cookie_preserve_oailb:false` 继续生效；策略或 owner 变化会重建 jar。HTTP 与 WSS 按同一 auth.ID 使用同一 jar。 |
| WSS 握手 | Cookie 作用域映射到 HTTPS；显式 Cookie 优先；成功和失败握手均采集允许的 Set-Cookie，下游不透出 Set-Cookie。保留标准 Domain、Path、Secure、Expires 规则。 |
| WS 复用 | 连接增加凭据及稳定握手条件的摘要。同 auth.ID 刷新 token、实际 owner 或关键握手配置变化时重连；普通 turn、Cookie 更新不会单独导致重连。需原连接的增量请求沿既有 replay-required 路径处理。 |
| owner 隔离 | 沿既有认证更新流程关闭旧连接、清理 jar 和内存 turn state；turn state 缓存同时校验 owner，旧请求的迟到响应不能恢复已替换的缓存桶。 |
| 错误分类 | HTTP/SSE/WS 对 slow_down、credit_balance_exhausted、organization_spend_limit_exceeded、project_spend_limit_exceeded、bio_policy 保留上游 code/message 并分类。额度/限速为 429，bio_policy 为请求级 400。 |
| 图片结果 | Images API 最终结果、completed 和 partial_image 流事件保留上游实际 generation_id；缺失时省略，不使用 imagegen_request_id 替代。 |
| 既有能力回归 | 模型目录能力字段、旧 compact、新 compaction item、Alpha Search 入口、设备收敛、双系统凭据路由和票据行为继续沿用。 |

## 2. 统一使用已有图片开关

```yaml
disable-image-generation: false
```

不增加新的 Lite 注入配置。原有 `disable-image-generation` 同时控制官方和兼容客户端：

| 值 | 非 Lite 对话请求 | Lite 对话请求 |
| --- | --- | --- |
| false（默认） | 缺少图片声明时补 hosted image_generation | 缺少图片声明时在 additional_tools 中补 image_gen.imagegen |
| true | 删除图片声明及关联 tool_choice，Images API 维持禁用 | 删除 hosted、平铺及 namespace 图片声明和关联 tool_choice |
| chat | 对话中删除图片声明，Images API 继续允许 | 对话中删除图片声明，Images API 继续允许 |
| passthrough | 不主动增删、转换工具声明 | 不主动增删、转换工具声明 |

Free 凭据和 spark 模型继续跳过自动图片注入。已有图片工具会参与去重；Lite 中已有 hosted 声明转换为函数声明不属于新增工具能力。旧 compact 路径只做禁图清理，不自动添加生成工具。

执行顺序为：完成翻译和 payload 配置 → 识别 Lite 意图 → 按图片开关归一化工具 → 根据最终工具列表判断是否自动补 Lite Header / WS 镜像。图片开关不会单独将普通请求切换成 Lite。

Lite 意图既可由已有 Header、WS 镜像或显式配置提供，也可从官方 body 标记、模型能力、all_turns 与布尔 false 恢复。因此 new-api 过滤请求 Header 时仍能正确选择 namespace 图片函数，不会先加 hosted 图片工具再破坏 Lite 条件。显式配置的 Header 覆盖参与注入前的判断，最终请求沿用相同优先级。

已有完整工具 schema 优先，不重复添加。检查覆盖顶层 tools 和 input.additional_tools，以及 namespace、点号/双下划线平铺函数名。tool_choice 的直接选择和 allowed_tools 列表随转换同步；不把 auto 改成 required，不强制模型调用图片。

搜索与图片开关分离：已声明 hosted web_search/preview 的 Lite 请求转换成 web.run；已有 web.run 保留完整 schema；没有搜索声明就不新增。禁图不删除搜索；passthrough 不进行工具转换。

新增或改变工具列表时使用稳定 item ID；原有内容不变的 item 保留 ID，相同内容重试保持稳定。历史调用、call_id、参数和多模态工具结果保留。

Lite 的 image_gen.imagegen / web.run 由调用方现有执行器执行。CPA 本次负责声明和协议转换，没有增加自动执行、续轮的工具循环。

## 3. 保留的本地定制行为

- official 判断仍仅使用 body 中的 Codex turn metadata 标记，可适配 new-api 丢失 Header 的转发。
- 设备收敛、Windows/macOS 凭据选择、单端凭据回退及日志布局保持既有配置语义。图片自动注入按第 2 节统一，不再跳过官方客户端。
- 同 turn 收到新的 turn state 时覆盖旧值并刷新原有 TTL；没有改成官方 CLI 的 OnceLock 行为。
- 现有 780 字节票据保留、定时续期及覆盖规则没有调整。票据探测只同步默认版本与凭据平台 UA。
- `__cf_bm` 等原有 Cookie、usage/apps 预热、16 分钟刷新及手动余量请求继续使用现有 jar；本次接入 WSS 并增加默认允许的 `__oailb`。
- 旧 `/responses/compact` 入口继续保留；新 compaction item 通过现有 Responses 通道保留。
- 没有新增 safety_identifier 注入、Guardian、workspace discovery、认证产品或存储系统。

## 4. 验证结果

| 检查 | 结果 |
| --- | --- |
| 修改的 Go 文件执行 gofmt | 通过 |
| `go test ./...` | 通过 |
| `go test -race ./internal/runtime/executor/... -run 'TestCodex156\|TestCodexUnifiedImagePolicy\|TestCodexToolPolicy\|TestCodexTurnState\|TestCodexCookie'` | 通过 |
| `go build -o test-output ./cmd/server` | 通过，临时构建产物已删除 |
| `git diff --check` | 通过 |

新增验收覆盖四种图片模式、官方/非官方、原有声明去重、Free/spark/compact 例外，以及 HTTP/SSE/WS/WS streaming 的 Lite 声明与最终 OAuth 请求、配置优先级、namespace 往返、Cookie 域/路径/过期/owner 隔离、token 更新后重连、新错误分类、generation_id 以及模型目录未知字段。

按升级指南的完成等级：**A（协议与格式兼容）已完成并通过本地测试；B（真实客户端图片/搜索执行闭环）本次未执行**。测试使用本地 HTTP/WebSocket 测试服务，没有进行付费模型、图片或搜索调用。

本次改造基于 main，随代码一起提交并通过新 tag 触发既有 Docker/Release 工作流。原升级指南仍保留在本地文档目录，本实施记录随代码发布。

## 5. Lite 保留工具 schema 修复

原先注入的图片与搜索模板来自升级指南中的手写示例，没有经过 CLI 最终 schema 解析和序列化。它们不能用作生产环境的保留函数定义。本节记录后续修复，原始升级指南保留原样。

- 图片与搜索默认声明改为固定 `rust-v0.156.0` 源码实际导出的完整 namespace JSON，保留完整 description、parameters、strict 与字段缺省语义。
- 导出在独立临时 Rust 工程中运行，使用原始 schema 生成代码、完整 parser/compaction 模块与 Lite serializer。所有发布依赖的版本和 checksum 均与官方 Cargo.lock 比对一致，最终执行 `cargo run --locked`。CPA 构建和运行均不依赖 Rust。
- 使用到的 18 个源码文件的哈希已逐个与官方固定提交核对；产物、锁文件、哈希记录和可复现导出脚本保存在 `internal/runtime/executor/helps/codex_lite_fixtures/`。
- 另外独立导出 0.154.0 的原生定义，避免将已知旧版原生图片声明误判为冲突；新注入仍使用 0.156.0。
- 已有保留函数不再仅凭同名跳过。匹配已验证 CLI 定义时原样保留；完全匹配已知旧 CPA 模板且上下文确定是新建时替换为正确定义；未知同名定义返回明确的请求级 400。
- 旧模板若关联 previous_response_id、已分配 ID 的 additional_tools 或助手/工具/推理历史，则返回 `codex_reserved_tool_schema_legacy_context`，要求新上下文或完整重建重放。不会在旧 item ID 下修改声明，也不会丢弃历史。HTTP 清理 previous_response_id 前的原请求同样参与检查。
- 这些规则只作用于 Lite 保留函数。非 Lite hosted image_generation、普通自定义函数的长度/数值约束、禁图与 passthrough 设置保持既有语义。

本地验收包含 gpt-6-astra / gpt-5.6-sol 的 HTTP、HTTP streaming、WebSocket、WebSocket streaming 最终出站声明与官方导出产物的完整结构比较。按用户要求，不进行真实上游模型调用，由用户部署后验收 gpt-6-astra。

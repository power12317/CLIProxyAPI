# 门票移出 auth 与 fork 新增写入路径确认单

日期：2026-10-20。

## 本轮授权与实现

本轮在 CPA `main`（基于 `2b1de245`）和 `codex/native-model-runtime`（基于 `895d3763`）实现门票移出 auth。
没有修改 codex-server 任何分支，没有改变它的原生 OAuth 刷新；没有改动其他 CPA 分支。
用户已确认本次只解决门票落盘，其余写入路径全部保留；不改变原版 CPA 的通用保存行为。

已实现：

1. 门票从 `Auth.Metadata` 改为独立进程内存缓存，按凭据 ID、所属身份指纹、注册代次和模型隔离。
2. 删除门票 Recorder → Manager.UpdatePreparedAuth → 凭据保存回调；正常响应及主动探测直接更新内存。
3. 状态展示、所有模型缓存与后续注入读取同一内存缓存，配置格式与状态接口结构不变。
4. 凭据重载继续使用同一注册代次的缓存；凭据删除会清理缓存，进程重启不从 auth 恢复门票。
5. OAuth token 正常续期不因为 token 字符串变化而换掉所属身份指纹；没有修改刷新请求及原生续接行为。
6. 凭据 metadata 规范化、常规 token 序列化和共享凭据序列化都过滤 `codex_turn_ticket:*`，包含旧快照内嵌的 Cookie 信息。
7. 服务启动后、启动本服务的 worker/文件监视之前，只对 auth 目录中含旧票据字段的 JSON 做一次清理。
   没有旧字段的文件不重写；重复启动不重复写干净文件。只移除票据字段，其他 token 和用户字段保留。
   这项清理属于用户本轮明确授权的“从 auth 文件去除门票”，不属于下列已经确认保留的其他逻辑。

启动清理会汇总输出清理数量，不打印票值或 Cookie。测试和构建没有运行生产服务，也没有访问、清理用户的真实 auth 目录。
普通响应不因票据更新而调用文件保存。下面第 1 项共享保存及其他正常凭据写入路径按用户确认保持原样；WRITE 日志本身不代表门票写入文件。

## 官方对照基线

使用这两条分支与官方 upstream/main 的共同基线：

```text
57bde35179ecbdca176ca8923d39cc18805774f2
fix(executor): preserve upstream usage on apply patch failures
```

以分支已提交代码与该官方提交的函数/文件差异确认来源；不把仅移动文件位置或新增保护判断误当作新增写入入口。

原版 CPA 已有、按要求保留的内容包括：

- `Manager.MarkResult`、注册、更新、刷新等流程调用凭据存储。
- 普通 `FileTokenStore.Save` 的 metadata 分支与 TokenStorage 分支。
- `CodexTokenStorage.SaveTokenToFile`。本轮之前，此文件与官方基线的 blob 完全一致：`05d95d9b4728c2a832ebf4a8a170898b2f788220`。
  上次把它当成 fork 新增原因讨论不够准确；它是原版已有的保存方式，本次没有改变其写盘条件。
- 原有管理端上传/编辑/启停、OAuth token 保存、插件凭据保存以及 Git/Postgres/Object 存储路径。
- 原有 Home 相关凭据入口。fork 新增的会话路由或 Cookie 预热不因此变成新增 auth 写入。

本轮只在公共序列化边界增加门票字段过滤，这是执行门票移除要求，不是改写原版的 OAuth 保存策略。

## 其他写入路径（用户确认保留）

下列编号在两个 CPA 分支含义一致。用户确认不再调整这些路径，全部保留；以下删除影响仅作为审查记录。

| 编号 | fork 新增逻辑 | 触发条件 | 对 auth 的作用 | 删除或还原后的影响 |
|---|---|---|---|---|
| 1 | 共享凭据保存分支 | 文件有 `codex_cli` 对象时，普通结果/状态保存也会进入，`enabled=false` 仍进入 | 读取现有文件，保留文件中的 token，合并 CPA metadata 与 disabled，再无条件写回 | 能去掉这条 fork 重复写入路径；必须同时处理第 3 项的 token 提交方式，防止旧 CPA 快照覆盖 codex-server 刚刷新的 token |
| 2 | Codex 模式与账号选择同步 | 启动、切换 Codex 模式、改变选择的凭据；状态真的不同或需要移除旧 owner/worker 标记时 | 修改 `codex_cli.enabled`，移除旧 `codex_cli.owner/worker_id`，写回原文件 | 现有 codex-server 按文件标记判断 worker 是否启用，直接删除会影响启停和账号选择；不新增协议的前提下不能假定它完全无用 |
| 3 | CPA 刷新后的额外 SaveTokens | CPA 自己负责 OAuth 刷新且取得共享文件路径时 | 在原版 Manager 保存前先直接写入 token 字段；之后仍有通用保存 | 去掉额外一次写入，但若单独保留第 1 项，它会从旧文件保留旧 token，因此两项需要一起决定 |
| 4 | 刷新时自动补客户端系统 | CPA 刷新成功，原 metadata 没有有效 `codex_client_system` 时 | 补 `codex_client_system=mac`，随正常保存落盘；不是独立每请求写入 | 可以去掉自动补字段；缺失字段时现有读取逻辑仍默认 mac，已有 windows 值不受影响 |
| 5 | Codex OAuth 临时占位文件 | 用户通过 CPA 发起一份新的 Codex runtime OAuth 登录 | 新建 UUID 名称的 JSON，占位字段含 type/auth_kind/codex_client_system；随后第 2 项启用 worker | 这是新建，不是后台覆盖现有账号。直接删除会影响现有按凭据文件启动 worker 的新账号 OAuth 流程 |
| 6 | Codex OAuth 完成后写正式名称文件 | 新账号 runtime OAuth 完成 | 从 worker 已写好的占位文件取凭据，经原版 saveTokenRecord 保存成账号命名文件；同名账号可能更新原文件 | CPA 不再完成当前文件命名/合并步骤，可能保留 UUID 文件或改变重复登录行为；不会删除 codex-server 原生刷新实现 |
| 7 | OAuth 占位文件停用及删除 | 第 6 项完成之后 | 先写 `codex_cli.enabled=false`，通知现有 worker 停止，再删除占位文件 | 与 5、6 是同一生命周期；只删停用步骤而继续删文件，可能留下仍在使用该占位路径的 worker，需成组决定 |

### 代码定位

1. `sdk/auth/filestore.go`：`shared` 判断及保存分支；`internal/auth/codexshared/file.go`：`MergeMetadata`、`Write`。
2. `internal/runtime/executor/helps/codexruntime/shared.go`：`reconcileState`，调用 `codexshared.Set/Write`。
3. `internal/runtime/executor/codex_executor_auth.go`：`Refresh` 末尾 `codexshared.SaveTokens`。
4. 同一 `Refresh` 中默认 `codex_client_system=mac` 的赋值。
5. `internal/api/handlers/management/codex_runtime.go`：`requestCodexRuntimeToken` 的 `target == nil` 分支。
6. `internal/api/handlers/management/codex_runtime_oauth_naming.go`：`completeRuntimeOAuth` 调用 `saveTokenRecord`。
7. 同一函数随后停用 placeholder、调用现有 `cpa/credential/reload` 和删除 placeholder 的步骤。

第 1 项是此前在“门票一直关闭”场景下复现的高频无变化重写路径。
第 4 项只是随真正 token 刷新保存的附加字段；第 5～7 项由用户 OAuth 操作触发，不能用来解释普通会话中不断出现的 WRITE。

`internal/auth/codexshared.Write` 是上述几项共用的底层写入器，不再单独重复编号；本次仅加入门票字段过滤，不删除其调用方。

## 已排除的误判

- `CodexRuntimeLog` 回传观察、原有 turn 状态缓存与 Cookie Jar 没有直接写 auth 的调用，不列为需要删除的写入入口。
- fork 增加的旧凭据版本防护、刷新所有权判断、请求路由选择不新增 auth 写调用；其中一些反而阻止旧请求更新凭据。
- 普通 `MarkResult` 保存属于原版；本次不会为了消除所有 WRITE 而删除它。
- codex-server 的原生 OAuth 保存与刷新完全不在本次删除范围。

## 验证

两个分支均完成：

- 门票正常响应、主动探测、所有模型缓存、凭据重载、过期续期和状态展示回归。
- 内存票据不改变 Auth.Metadata，不调用旧凭据保存回调；不同凭据、身份、注册代次和模型隔离。
- metadata、共享保存、TokenStorage 直接保存和共享 token 刷新均不序列化旧票据快照。
- 旧文件清理保留其他字段，包括大整数、客户端配置与 token；再次清理不改变文件 mtime。
- 相关测试 `-race`、`gofmt -w .`、`git diff --check` 和服务构建通过。
- 两分支整仓 `go test ./...` 排除此前已确认的 `TestCodexRuntimeStandardStatusMapsResultsAndSyncsOriginalFile` 基线失败用例后通过。

扩大 auth 同步竞态补测时，`TestHandleAuthUpdates_SameRevisionWaitDoesNotWaitForOtherAuthInBatch`
曾发生一次 2 秒等待超时；用修改前的 `service_auth.go` 作 overlay 复测也出现相同失败。
当前修改版的该用例连同门票专项复测通过。没有为此更改原有 auth 同步逻辑或隐藏该失败。

用户已确认收口为门票仅存内存、清除旧票据字段并防止重新写入；上列 1～7 项保留。

对应发布标签：main 为 `v2026.10.20-1`，native 分支为 `native-runtime-v2026.10.20-1`。

# oailb 借用：CPA 实验功能与 CPAMP 对接

本功能在 CPA `codex/oailb-borrow` 测试分支开发，不默认合入 main。CPAMP 的适配由其项目开发负责，使用独立测试分支。

## 最终规则

- 在当前实例的 Codex Header Defaults 内选择来源实例，再选择该实例的一份启用的 Codex OAuth 凭据，默认不借用。
- 当前实例保存来源连接地址、管理密码、稳定凭据 ID 和展示信息；不依赖 CPAMP 持续运行。
- 仅覆盖 `https://chatgpt.com/backend-api/codex/responses` 的 HTTP/SSE 请求以及对应 WSS 握手中的 `__oailb`。
- 其他 Cookie 保持当前实例当前凭据自己的值。本地 Jar 始终正常接收上游 Set-Cookie；借用值不写入本地 Jar。
- 只解析 JWT `exp`。`exp - 300 秒` 前固定复用；从该时刻开始按请求尝试刷新；刷新失败时旧值可继续用到 `exp`；到 `exp` 后使用本地 Jar。
- 不另外保存 Cookie 生成时间或到期时间，不解析 JWT host 作为请求地址，不修改 JWT。
- 初次获取失败、来源不可达、鉴权失败、来源凭据被删除/禁用/没有 Cookie，都回退本地，不因借用失败阻断模型请求。
- 获取最多等待 3 秒，失败或仍在刷新窗口时最多每 30 秒尝试一次，同来源并发获取合并。
- 来源只读取指定凭据的本地 Jar，缺失或进入刷新窗口时按需请求 usage。无需开启 turn-state。既有 16 分钟 usage 刷新保持。
- 不登记借出用途，不建立后台刷新/保活任务，不共享来源 OAuth token、其他 Cookie 或整个 Jar。
- WebSocket 仅在首次建立连接或原有机制重连时检查、刷新和携带借用 Cookie。健康连接持续复用，Cookie 到期、借用值变化或借用配置变化都不会主动断开；后续 response.create/steer 不触发借用获取。SSE 仍按每次 HTTP 请求检查。

## CPAMP 页面与保存

当前页面已经属于实例 5，因此只有“来源实例 → 来源凭据”两级选择，没有再选择借用方的控件。多个实例可以分别配置同一个来源。

1. 从 CPAMP 的实例列表选择来源，排除当前实例。
2. 通过已有凭据接口 `/v0/management/auth-files` 读取来源凭据。
3. 使用 `provider=codex`、OAuth、未禁用的凭据。展示 `name`/凭据文件名，提交稳定 `id`，不得提交列表序号或 `auth_index`。
4. Manager 后端解析已保存来源连接的真实管理密码，下发给当前 CPA。浏览器不用再次输入来源密码，也不需要接收密码。
5. 保存成功后显示选中的来源实例与凭据；来源变更时必须重新选择该来源的凭据。
6. 关闭使用空对象 `{}`。该修改只作用于当前实例。
7. 能力接口为 404/405/501 时视为旧 CPA 不支持；不要影响现有 Header Defaults 和其他配置。

## 读取能力和设置

`GET /v0/management/codex-oailb-borrow`

沿用当前实例管理鉴权。无配置：

```json
{"supported":true,"config":null}
```

已配置：

```json
{
  "supported": true,
  "config": {
    "source-instance-id": "instance-9",
    "source-url": "https://cpa9.example.com/prefix",
    "source-management-key": "enc:v1:<nonce>:<ciphertext>",
    "source-auth-id": "<auth-files 返回的 id>",
    "source-auth-file": "codex-user-windows.json"
  }
}
```

Manager 给选择器返回来源 ID、凭据 ID/文件名即可，不把来源密码返回给浏览器。CPA 的密文可以原样往返。

## 保存设置

`PUT /v0/management/codex-oailb-borrow`

请求体为上述 `config` 对象本身，使用连字符字段名。配置来源时必须提供 `source-url`、`source-management-key`、`source-auth-id`。

```json
{
  "source-instance-id": "instance-9",
  "source-url": "https://cpa9.example.com/prefix",
  "source-management-key": "<Manager 取得的来源实例管理密码>",
  "source-auth-id": "<来源凭据 id>",
  "source-auth-file": "codex-user-windows.json"
}
```

关闭请求体：`{}`。成功响应：`{"status":"ok"}`。

配置在当前实例的 `codex-header-defaults.oailb-borrow` 下保存；`GET /config` 和 `GET/PUT /config.yaml` 同样支持。YAML 编辑器原样保留密文；提交明文时 CPA 在落盘前加密。

密码使用 AES-256-GCM，格式 `enc:v1:<base64 nonce>:<base64 ciphertext+tag>`。CPA 自动在持久化 auth-dir 中创建 `.oailb-data-key`，文件权限 0600。不使用管理密码的 bcrypt 哈希，也不复制 CPAMP 密文或共享 CPAMP 数据密钥。部署迁移时保留当前实例的 auth-dir。读取不到解密密钥时运行请求回退本地 Jar。

`source-url` 支持实例根地址、反向代理子路径，也兼容以 `/v0/management` 结尾的地址。该地址必须能由借用方 CPA 访问。

## CPA 之间的借出接口

`POST /v0/management/codex/oailb/borrow`

使用来源实例的管理鉴权，远程访问遵循现有管理访问策略。

```json
{"auth_id":"<指定凭据 id>"}
```

有效响应：

```json
{
  "available": true,
  "value": "<__oailb JWT>",
  "expires_at": "2026-09-26T12:00:00Z",
  "remaining_seconds": 1800
}
```

不可用响应：`{"available":false}`。Cookie 的最终有效性由借用方解析 JWT `exp` 判定；返回时间字段只供接口使用，不另作续期依据。再次读取不会延长 JWT。

该接口不供前端展示 Cookie。没有可借值时仅按需尝试 usage 刷新，不触发门票探测、不创建常驻任务。来源凭据必须精确匹配，找不到时不得替换成其他凭据。

## 验收重点

- 当前实例保存选择，其他实例配置不变；来源凭据切换、清空及旧版兼容正常。
- 配置文件不保存来源管理密码明文，重启后可解密，YAML 往返不丢其他配置和注释。
- exp-300 前不频繁拉取；缓冲期获取失败保留旧 JWT；exp 后回退本地；恢复后重新借用。
- HTTP 实際发送只有一个 `__oailb`，其他本地 Cookie 保留，响应 Cookie 更新不覆盖借用缓存。
- WSS 握手与 HTTP 使用同一个借用缓存；已建立的普通/topic/双工连接不因 Cookie 到期或借用变化而重连，也不额外访问来源接口。自然重连时使用当时有效的借用值或本地 Jar。
- 来源端只刷新选中凭据；未开启 turn-state 时 usage 按需刷新仍可工作。
- 测试仅用本地模拟上游，不调用真实账号或 ChatGPT 模型。

## 实验分支发布

- Git 分支：`codex/oailb-borrow`。
- 分支 tag：`oailb-borrow/vYYYY.MM.DD-N`，首版为 `oailb-borrow/v2026.09.26-1`。
- 推送该 tag 触发 `docker-oailb-borrow`，构建 Linux amd64/arm64 多架构镜像。
- 镜像独立存放在 `ghcr.io/power12317/cliproxyapi-oailb-borrow:vYYYY.MM.DD-N`。
- 实验 tag 排除在通用 Release 流程外，不更新正式镜像的 `latest`。

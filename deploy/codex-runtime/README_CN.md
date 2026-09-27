# 方案 B v2：CPAMP 管理与共用凭据

本版改为 TCP WebSocket，不再共享 Unix socket。CPA 与 Codex 直接使用同一份
CPA 格式凭据文件；CPAMP 负责开关、授权链接和回调提交。

## 部署

准备匹配分支构建的镜像：CPA 的 codex/plan-b-managed-auth、Codex 的
codex/cpa-managed-auth，以及 CPAMP 的 codex/codex-runtime-control。
Codex 镜像构建方法见其仓库 deploy/cpa-runtime/README.md。普通官方镜像
不含桥接功能。建议在开发机或 CI 构建，服务器只使用已准备的镜像。

在服务器现有 Compose 的 .env 中填写：

~~~~dotenv
CPA_RUNTIME_IMAGE=your-tested-cpa-image
CODEX_RUNTIME_IMAGE=your-tested-codex-image
CODEX_BRIDGE_TOKEN=your-bridge-key
~~~~

沿用现有 CPA Compose，再叠加本目录覆盖文件：

~~~~sh
docker compose -f docker-compose.yml -f deploy/codex-runtime/compose.override.yaml up -d
~~~~

覆盖文件没有启动 command 参数列表，也没有 socket 卷或额外公网端口。Codex
镜像入口自动读取环境变量，第一台默认监听 127.0.0.1:38317/cpa/v1/ws。
18317 属于 CPAMP，不用于 Codex。

worker 使用 network_mode: service:cli-proxy-api，与 gost 一样共用 CPA 的网络
命名空间及出口。extra_hosts、DNS 和 IPv6 配置放在 CPA 主服务，worker 不重复
设置 ports/networks/extra_hosts。

auths 目录在两边共享，Codex 其他数据保存在独立 CODEX_HOME 卷中。覆盖文件
默认以 root 匹配原 CPA 容器，读取其 0600 凭据；若 CPA 使用其他 UID，两边改为
相同 UID。挂载整个目录，避免单文件挂载看不到 Codex 替换后的文件。

## CPAMP 操作

打开“Codex 运行时”页面，第一台 worker 填写：

| 字段 | 示例 |
| --- | --- |
| ID | worker-a |
| 地址 | ws://127.0.0.1:38317/cpa/v1/ws |
| 凭据文件 | worker-a.json |
| 连接密钥 | 与 CODEX_BRIDGE_TOKEN 相同 |
| 模型 | 可留空，使用原 CPA 模型集 |

文件名必须与容器 CODEX_CPA_AUTH_FILE 的文件名一致。使用已有凭据时两处均填
现有文件名；新增授权时会创建指定文件。

开启总开关，点击官方授权，在浏览器完成授权后把完整回调链接粘贴回 CPAMP：

~~~~text
CPAMP → CPA → Codex 生成官方授权链接
用户完成授权并粘贴完整回调链接
CPAMP → CPA → Codex 完成授权码交换 → 写回同一份凭据
~~~~

不是设备码流程，不需要进入容器执行登录命令。授权状态可用“检查授权状态”
按钮查询；不增加后台探测、请求数量或容器管理功能。

## 切换

凭据新增字段：

~~~~json
"codex_cli": {"enabled": true, "worker_id": "worker-a", "owner": "codex"}
~~~~

总开关开启且凭据启用时由 Codex 调用和刷新。关闭总开关、关闭单凭据或移除
worker 后，owner 回到 cpa，取消旧请求，CPA 重新读取同一文件并恢复原生调用
和刷新。总开关关闭保留单凭据的 enabled 偏好。

关闭状态不向 Codex 发 RPC、探测或重连。页面只显示 CPA 本地文件状态；
授权和测试按钮不可用。Codex 容器可常驻，但不使用/刷新 CPA 所有的凭据。

按用户要求，不新增跨进程锁、租约、epoch、CAS 或切换握手，不保证在进行的
刷新与切换严格互斥。额外保护措施需在功能完成后另行获得用户同意。

## 多账户、升级与测试

每个账户使用独立 worker 容器、固定凭据文件和 CODEX_HOME。第二台使用不同
ID/文件/数据卷，并设置 CODEX_CPA_PORT=38318；CPAMP 中填写对应地址。

目前支持普通 Responses HTTP/SSE 和 CPA 现有入口转换。工具调用回传给调用者，
不在 Codex 执行或自动续跑。compact、持久上游会话、上游 WebSocket steering、
hosted tools、特殊图片/搜索接口和 Home 尚未接入。共享凭据以本地文件存储
为基础，PG/git/object 远程存储不在本轮范围内。

升级 Codex fork 后，先运行其相关测试，再运行 CPA 跨仓库测试：

~~~~sh
CODEX_RUNTIME_TEST_BINARY=/absolute/path/to/codex-app-server \
  go test ./internal/api/handlers/management -run '^TestCodexRuntimeForkV2' -count=1 -v
go test ./...
go build -o test-output ./cmd/server && rm test-output
~~~~

测试使用临时目录、假 token 和本地模拟上游，不访问真实账户。
精确接口合同见 PROTOCOL_V2.md。VALIDATION.md 是旧 v1 的历史验收记录。

# Codex Runtime 方案 B：部署与验收

这是实验性接入，需要同时使用 CPA 的 `codex/plan-b-managed-auth` 与 Codex fork 的 `codex/cpa-managed-auth`。普通官方 Codex 镜像没有 `cpa/*` 接口，原有 CPA `latest` 镜像也没有此执行器。

CPA 负责入口协议、模型路由和账号选择；每个独立 Codex worker 负责官方 OAuth 登录、原生凭据存储、刷新、安装身份、上游 HTTP/SSE 和响应处理。CPA 只持有 worker 引用，不挂载、读取或写入其 `auth.json`，也不会调用自己的 OAuth 刷新器处理这些引用。原有 `codex` 凭据与新 `codex-runtime` provider 分开注册。

## 配置与启动

1. 在 Codex fork 仓库构建镜像，使用该仓库的 `deploy/cpa-runtime/Dockerfile` 和说明。镜像保留官方 CLI 登录及 app-server 所需 companion。生产构建记录上游提交、fork 提交、镜像 digest；本地标签仅用于开发。
2. 在 CPA 本分支构建自己的镜像。原有 Compose `build` 可以使用本分支代码：`docker build -t cpa-codex-runtime:local .`。设置 `CPA_RUNTIME_IMAGE=cpa-codex-runtime:local`，`CODEX_RUNTIME_IMAGE=codex-cpa-runtime:local`。发布环境使用事先拉取并验证的 digest；示例 `pull_policy: never` 防止自动升级未验收版本。
3. 将以下片段合并到自己的 CPA 配置。替换账户与模型，不要直接复制占位符。模型必须显式配置，`prefix` 用于区分已有 provider；测试时请求 `runtime/实际模型名`。如需只发布带前缀模型，启用 CPA 现有 `force-model-prefix: true`。

```yaml
codex:
  runtime:
    enabled: true
    workers:
      - id: worker-a
        socket: /run/codex-worker-a/cpa.sock
        account-id: YOUR_CHATGPT_WORKSPACE_ID
        models: [YOUR_EXPLICIT_MODEL_ID]
        prefix: runtime
        disabled: false
```

4. 从 CPA 仓库根使用已有 Compose 加本目录覆盖文件。以下假设原文件名为 `docker-compose.yml`；名称不同则替换第一项。

```sh
export CPA_RUNTIME_IMAGE=cpa-codex-runtime:local
export CODEX_RUNTIME_IMAGE=codex-cpa-runtime:local
docker compose -f docker-compose.yml -f deploy/codex-runtime/compose.override.yaml config
docker compose -f docker-compose.yml -f deploy/codex-runtime/compose.override.yaml up -d
```

5. 在 worker 内独立完成官方登录，不导入 CPA 已在刷新中的旧 OAuth grant。官方设备授权必须在账户设置允许的情况下使用；后续登录与账号查询方法以该 fork 的 CLI 帮助和 app-server `account/read` 为准。

```sh
docker compose -f docker-compose.yml -f deploy/codex-runtime/compose.override.yaml \
  exec codex-worker-a /usr/local/bin/codex -c 'cli_auth_credentials_store="file"' login --device-auth
docker compose -f docker-compose.yml -f deploy/codex-runtime/compose.override.yaml restart codex-worker-a
```

登录后重启 worker，确保常驻 app-server 载入新登录。`account-id` 是 ChatGPT 工作区/账户 ID，不是邮箱；CPA 在 capabilities 和推理开始处校验它。实际账户登录、真实模型调用及容器部署由操作者验收，本分支自动测试只使用假凭据与本地模拟服务。

## 隔离和生命周期

- worker 使用 `network_mode: service:cli-proxy-api`，与原有 gost 一样共用 CPA 网络命名空间和出口。无需额外 `ports` 或 `networks`。CPA 上挂的 IPv6 网络继续生效。
- Docker 的 `service:` 最终使用 `container:` 网络模式。`extra_hosts` 和 DNS 配置保留在 CPA 主服务，worker 不重复声明：Docker 在该模式下复用目标容器的网络文件，并禁止 worker 单独设置 `--add-host`、`--dns`、端口发布等选项。环境变量、应用配置与代理设置仍需按 worker 配置；CPA 的 `proxy-url` 不会自动变为 worker 的代理。参见 [Docker container networks](https://docs.docker.com/engine/network/#container-networks)。
- 每个账户一个 worker、一个私有 `CODEX_HOME` 卷、一个私有 socket 卷；CPA 只挂 socket 卷。镜像 worker UID/GID 为 `10001:10001`；复用已有卷时需保证其可写，socket 对 CPA 用户可连接。不要把整个 Codex home 挂给 CPA。
- 配置 `disabled: true` 立即停止新的 CPA 调度；移除 worker 条目会移除引用和模型。已有推理允许结束，worker 常驻进程及其已启用的后台生命周期继续运行。禁用不等于注销，也不自动停止容器。
- 增加账户时复制 worker 服务和两个卷，设置唯一 `credential_id`、CPA `id`、socket 路径和对应账户 ID。静态 Compose 不能按目录文件自动增删容器；本方案由部署配置显式管理数量，不向 CPA 提供 Docker socket。
- CPA 容器被重新创建时，应通过同一 Compose 项目一起重新创建依赖的 gost/worker，使其加入新的网络命名空间。`depends_on.restart: true` 覆盖 Compose 显式更新操作；不把它当成任意外部重建后的自动修复机制。
- 不删除官方已启用的后台任务，不伪造事件上报，也不承诺官方不存在的固定周期心跳。共享出口不会使容器身份变成同一安装。

## 当前能力与明确限制

支持普通 Responses HTTP/SSE，以及 CPA 的 OpenAI Chat、Claude、Gemini、Interactions 适配入口。原生 Responses 请求保留未知字段；worker 明确拒绝其不支持的语义。模型返回 function/custom tool call 时直接回给调用者，桥接不执行本地工具，不产生自动下一轮。

首版 `operations=["responses"]`、`persistentSessions=false`。不支持 compact、上游 WebSocket steering、`previous_response_id`、`conversation`、`generate`、启用 `background/store`、调用者身份/传输元数据（包括 `client_metadata`）、hosted tools、独立图片/搜索接口、任意管理 APICall、token counting 和 Home 分发。不能因普通 CLI 支持某功能就假设桥接也支持。CPA 会检查能力，worker 会校验字段；不静默回退旧 executor。

CPA 不重放身份不匹配或状态不明的请求。Codex 的官方 401 恢复发生在推理被接受之前：可能先重载本地凭据再刷新，只有 Codex 写回 refresh token。连接取消只取消对应请求，不停止 worker。不设置生产推理连接建立后的 read/idle/total timeout。

原生 Responses 流保留完整 JSON 事件与未知字段；同步请求返回完整终态 response。跨协议调用仍服从对应协议的表达能力及 CPA 已有转换器，不能宣称字节级等同原始 Responses。

## 回归和升级

```sh
go test ./internal/runtime/executor/helps/codexruntime
go test ./internal/runtime/executor -run TestCodexRuntime -count=1
go test ./internal/config ./internal/watcher/synthesizer ./sdk/cliproxy/auth ./sdk/cliproxy
CODEX_RUNTIME_TEST_BINARY=/absolute/path/to/codex-app-server \
  go test ./internal/runtime/executor -run '^TestCodexRuntimeForkContract$' -count=1 -v
go test ./...
go build -o test-output ./cmd/server && rm test-output
```

双仓库测试启动真实 fork 进程，使用临时 HOME、合成 OAuth 文件、本地 refresh/Responses 服务，检查刷新所有权、请求/事件保真、工具回传和 worker 存活。未设置二进制路径时该集成测试明确 skip，普通单元测试使用 Unix socket mock。

后续升级在 Codex fork 同步官方后运行其桥接、AuthManager、Responses、取消与生命周期测试，再运行上述 CPA 双仓库测试。IPC major 不匹配、身份不匹配或能力缺失均拒绝接入。通过后记录并固定新镜像版本；失败继续使用上一组已验证镜像。协议兼容不能替代行为回归。不得自动部署上游 main 或浮动 latest。

方案说明分别在 [PLAN_B.md](PLAN_B.md)、[PLAN_A.md](PLAN_A.md)，共享协议在 [PROTOCOL.md](PROTOCOL.md)。方案 A 保留独立分支基线，等待方案 B 真实测试后再决定实现。

# CPA 直接部署

本目录已经包含编译好的 Linux 动态库，不需要在服务器安装 Go、Docker，也不需要执行脚本。

## x86_64 / amd64

把下面文件复制到每个 CPA 的插件目录：

```text
plugins/linux/amd64/codex-turn-state.so
```

服务器上的最终路径：

```text
/你的CPA目录/plugins/linux/amd64/codex-turn-state.so
```

## ARM64 / aarch64

使用：

```text
plugins/linux/arm64/codex-turn-state.so
```

不要把 amd64 和 arm64 文件混用。用 `uname -m` 检查服务器架构：`x86_64` 对应 amd64，`aarch64` 对应 arm64。

## 配置

把 `config.snippet.yaml` 中的 `codex-turn-state` 节点合并进 CPA 的 `config.yaml`，并确认：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    codex-turn-state:
      enabled: true
```

配置文件改完后重启：

```bash
docker compose restart cli-proxy-api
```

检查日志：

```bash
docker logs cli-proxy-api 2>&1 | grep -i codex-turn-state
```

应看到 `plugin loaded` 和 `configured`。如果仍然显示未注册，优先检查 `.so` 是否放在容器实际挂载的 `/CLIProxyAPI/plugins/linux/amd64/` 或 `/CLIProxyAPI/plugins/linux/arm64/`，以及 `plugins.enabled` 是否为 `true`。

重启后进入：

```text
http://服务器地址:8317/v0/resource/plugins/codex-turn-state/dashboard
```

先完成一次探测并切换 `role: business`，模板才会开始替换业务请求。

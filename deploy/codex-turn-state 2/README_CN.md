# 安装

适用于 CPA v7.3.4+、项目自带 Docker Compose 部署，服务器需要 Docker 和网络。

1. 将 `codex-turn-state` 整个目录放到服务器 CPA 根目录，与 `docker-compose.yml`、`config.yaml` 同级。

2. 在 CPA 根目录执行：

   ```bash
   bash codex-turn-state/install.sh
   ```

3. 将 `config.snippet.yaml` 合并进 CPA 实际使用的 `config.yaml`。已有 `plugins:` 就在原节点内合并，保留其他插件，不要重复添加顶层 `plugins:`。

   - `probe_api_key`：填写 CPA 的 `api-keys` 中任意一把密钥。
   - `probe_management_key`：填写登录 CPA 管理页面的明文密码，不是 `$2…` 哈希。
   - 如果 CPA 容器内部端口不是 `8317`，同步修改 `probe_base_url`。

4. 执行：

   ```bash
   docker compose restart cli-proxy-api
   ```

5. 打开 CPA 管理页面的 **Codex Turn-State**，或访问 `http://服务器地址:8317/v0/resource/plugins/codex-turn-state/dashboard`。暂停 Codex 业务请求 → 选择账号和模型 → 保存探测范围 → 开始探测 → 等待目标全部就绪且账号状态恢复 → 页面切换 `role` 为 `business`，确认 `dry_run` 为 `OFF` → 恢复业务。

模板默认 1 小时失效；到期前需重复探测。再次采集时先暂停业务，在页面切回 `probe`，重启 CPA，再执行第 5 步的采集和切回业务操作。

上游插件页面和操作接口无登录鉴权，必须限制该插件路径仅允许管理人员访问。探测会临时切换账号启用状态并消耗上游额度。

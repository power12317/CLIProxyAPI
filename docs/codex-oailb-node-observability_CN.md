# Codex 上游节点日志与请求监控

## 保留范围

从 `codex/oailb-borrow` 实验中只保留 `oailb_node` 的采集、日志和现有请求监控数据传递，以及实际响应模型显示修复。代码以当前 `main` 为基础，保留主线已有的 WebSocket、Cookie Jar、凭据和配置行为。

不包含跨实例 Cookie 借用、来源实例或凭据选择、借用管理接口、借用缓存与刷新逻辑，也不包含实验镜像发布流程。无需新增配置开关或来源实例信息。

## 节点来源

- 优先解析本次上游响应 `Set-Cookie` 中的 `__oailb`；响应没有该 Cookie 时，读取本次实际发出的请求 Cookie。
- JWT 的 `host` 为 `chat.gateway.unified-195.api.openai.com` 时，只记录 `unified-195`。
- 响应明确携带无效或删除的 `__oailb` 时，节点留空。
- HTTP 在本实例 Cookie Jar 附加 Cookie 后读取；采集逻辑不修改请求或响应 Cookie，不请求其他实例。
- WebSocket 在握手时保存节点，连接复用及双工响应继续记录该连接的节点。后续 Cookie 变化不改变已建立连接的节点记录。

## 日志格式

```text
[2026-09-26 21:17:12] [8e520ac0] [codex-user-windows.json] [01a0dd74] [01a0dda3] [info ] [gin_logger.go:115] 200 | 12.884s | unified-195 | gpt-6-sol/gpt-6-sol | 780/780 |      172.25.0.1 | POST    "/v1/responses"
```

列顺序：`状态码 | 耗时 | 节点 | 请求模型/响应模型 | 请求/响应 turn-state 长度 | IP | 方法及路径`。

- 节点未知时为 `-`，不输出 `oailb_node=` 后缀。
- 凭据文件名、session/turn 前 8 位继续保留在日志前缀。
- 无需开启详细日志，不增加独立日志行。
- 响应模型记录上游实际返回值。后续 turn-state 更新不会清空模型；新的上游尝试会重置旧模型。

## CPAMP 数据契约

沿用现有 usage/request 记录、Redis/RESP 队列和插件记录，不新增存储系统：

```json
{"model":"gpt-6-sol","response_model":"gpt-6-sol","oailb_node":"unified-195","turn_state_len":"780/780"}
```

`oailb_node` 是可选字符串，未知时为空或省略。CPAMP 在请求监控的状态单元格新增一行节点名；响应模型继续使用现有 `response_model` 字段，不以请求模型补造。文本日志解析兼容新的固定节点列、旧的 `oailb_node=...` 尾部格式及无节点旧格式。

# 0.4.2 验收记录

日期：2026-11-06。本次为 0.4.1 的补丁版本发布，统一运行时、管理台、MCP、Compose 和镜像标签版本号；不改变数据库 schema 或业务策略。

- 版本号统一为 `0.4.2`，服务 `/healthz`、`/readyz`、管理台、MCP 实现和 Compose 使用同一版本。
- 镜像发布为 `allen0039/mail-auto:0.4.2` 和 `latest`，支持 Linux amd64 / arm64。
- 规则、任务、可选定时、外部 MCP 批次和 Telegram 通知行为沿用 0.4.1。

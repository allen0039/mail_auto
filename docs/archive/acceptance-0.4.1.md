# 0.4.1 验收记录

日期：2026-11-05。本次为 0.4.0 的补丁版本发布，统一运行时、管理台、MCP、Compose 和镜像标签版本号；不改变数据库 schema 或业务策略。

## 发布内容

- 版本号统一为 `0.4.1`，服务 `/healthz`、`/readyz`、管理台、MCP 实现和 Compose 使用同一版本。
- 镜像发布为 `allen0039/mail-auto:0.4.1` 和 `latest`，支持 Linux amd64 / arm64。
- 规则、任务、可选定时、外部 MCP 批次和 Telegram 通知行为沿用 0.4.0。

## 验证

- `go test ./... -count=1`、`go vet ./...`、`go test -race ./...`、`govulncheck` 和 Compose 配置检查通过。
- 前端 JavaScript 语法检查通过。
- Gitleaks 扫描无泄漏；公开仓库不包含实际部署节点、域名、路径、备份位置、实例标识、数据库或密钥。
- 发布镜像通过双架构清单、版本输出、非 root、只读根文件系统、`readyz`/`healthz`、匿名 API 和默认 MCP 边界验证。

## 运维边界

补丁版本不创建任务、不启用邮箱写入、不发送 Telegram 消息，也不替代真实服务商写入和无人值守外部客户端的专用验收。升级与回退仍需同时保留数据库快照、匹配主密钥、Compose 配置和旧镜像。

# 0.10.6 发布与 Oracle1 验收

日期：2026-11-22（Asia/Shanghai）。本版将 Gmail 接入改为 Google 应用专用密码，移除 Gmail / Outlook 邮箱 OAuth，并把数据库从 schema 14 迁移到 schema 17。

## 本地与远端验证

- Go race 全套、`go vet`、staticcheck、govulncheck 通过。
- 发布核对夹具 46 项通过，Compose 配置、Python 与 JavaScript 语法检查通过。
- GitHub Actions 的 `check`、`image`、`ui` 三个作业全部成功，UI 门禁覆盖 1280 / 375 / 390 px。
- 发布提交为 `0438758d5a982c2864953af0ce6012a64350d360`，标签为 `v0.10.6`。

## 镜像与 Oracle1

- `allen0039/mail-auto:0.10.6` 与 `latest` 的多架构 index 摘要均为 `sha256:5d03cd66078e19281f7aef185047d9bd9e0d3b28dd5f274eaa4d72af92fddaa5`，包含 Linux amd64 / arm64。
- Oracle1 运行版本 `0.10.6`，`healthz` 和 `readyz` 正常，schema 17，重启次数 0。
- 现有网易账号的 14/14 个文件夹完成同步，16/16 配置表保持一致，4/4 任务用户字段保持一致，主密钥核对一致。
- 部署后自然任务运行 1 次并以 `noop` 完成。该结果证明后台任务路径正常，不代表本次产生了新的邮箱写入。

升级期间创建的临时回退快照在验证完成后已删除。Oracle1 未保留本项目旧备份或旧 Mail Auto 镜像，其他服务资源未清理。

Gmail 真实账号的应用专用密码验证需由使用者在邮箱设置中完成，本次 Oracle1 验收使用现有网易账号。

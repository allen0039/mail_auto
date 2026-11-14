# 0.7.1 验证范围

日期：2026-11-14（Asia/Shanghai）。修复 0.7.0 独立审计的 MC-01～MC-05；数据库仍为 schema 13，无迁移。0.7.0 的验证范围见 [acceptance-0.7.0.md](acceptance-0.7.0.md)，相关用例全部保留。

## 修复与回归

| 编号 | 修复 | 回归 |
| --- | --- | --- |
| MC-01 | `Activate` 提交事务内用同一事务查询管理员版本与 `must_change`；`SaveConnection`、`DeleteConnection` 同样核对 | `agent.TestAdminChangeDuringCheckRejectsActivation`（检测期间改凭据、被要求改密两种情形；连接数、当前配置版本与模型不变；旧访问保存被拒） |
| MC-02 | 页面草稿单独记录载入时的连接版本与当前配置版本；保留草稿的刷新不更新它们；冲突时保留草稿并提示“取消修改”载入最新配置 | `scripts/ui-check/model-ui-check.js`：refresh keeps dirty draft / stale draft is announced / stale draft submits its original base version / external change not overwritten / draft kept after conflict / cancel loads the newer saved state |
| MC-03 | 检测与保存期间锁定表单（fieldset disabled）；成功响应仅在草稿代际未变时重载表单，否则保留新编辑并以本次提交产生的版本为基准 | 同脚本：fields locked during check / refresh during check keeps lock and draft / applied value shown after check / late success keeps newer edit / rebased draft applies without false conflict / fields locked during backup save / backup save shows saved value, current unchanged |
| MC-04 | 待生效判断在服务端解密后比较连接与当前配置的密钥（各自 AAD），不比较密文、不返回密钥或指纹 | `agent.TestKeyOnlyChangeIsPendingUntilApplied`（保存新密钥后列表与保存结果均为待生效；运行时仍用旧密钥；重复保存同一密钥不升版本；检测并使用后改用新密钥且不再待生效） |
| MC-05 | 网络策略载入后派发 `network-policies-rendered`，模型卡片重算摘要 | 同脚本：network policy summary after load / summary cleared for inherit |

新增 Go 回归在 0.7.0 代码上（用 overlay 替换 `connections.go`）均失败，修复后通过。审计提供的 `TestAuditActivationRejectsRevokedAdmin`、`TestAuditSavedKeyChangeIsPending` 修复后通过。

## 执行的检查

- `make check`：全项目 `go test -race`、`go vet`、gofmt、staticcheck 通过；另对 agent、admin、modelprovider、store 以 `-count=1 -race` 无缓存复跑通过。
- govulncheck：无被调用的漏洞；`docker compose config --quiet`、`git diff --check`、`CGO_ENABLED=0 go build -trimpath` 通过。
- 界面：`scripts/ui-check/model-ui-check.js`（假 API）在 1440、375、390 像素各 37 项全部通过，无横向溢出。审计 UI 反例三项在修复后通过；其中第一项原脚本假定提交成功，修复后提交按预期得到冲突，复验时对等待条件做了容错调整。

## 验证边界

单元与界面检查使用假模型和假 API。真实模型的列表获取与双连接切换需要生产管理员登录及第二个可用模型凭据；真实分类写入需要专用测试邮件。未具备时在发布报告中标为未验证，不以健康检查或 `noop` 运行代替。

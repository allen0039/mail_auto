# 0.7.2 验证范围

日期：2026-11-14（Asia/Shanghai）。隐藏模型连接高级设置中的“输出长度参数”，保留后端兼容能力与已有配置；数据库仍为 schema 13，无迁移、无新依赖。0.7.0、0.7.1 的验证范围见 [acceptance-0.7.0.md](acceptance-0.7.0.md)、[acceptance-0.7.1.md](acceptance-0.7.1.md)，相关用例全部保留。

## 变更

- 页面：删除 `#model-max-param` 标签与选择框。`agent.js` 用页面状态 `maxParam` 代替：`fillForm` 从所选连接载入 `max_tokens_param`（新连接为空），`draftBody` 原样提交；没有任何控件能修改它。停用 Agent 仍提交当前配置自身的值（未改动）。
- 摘要：值非空时高级设置标题旁显示“已启用输出长度兼容设置”。
- 提示文案：`provider_request_rejected`、`provider_output_truncated` 的中文提示不再指向已移除的选项（错误码不变）。
- 后端：无改动。`normalizeConfig` 继续接受空值、`max_tokens`、`max_completion_tokens`；`chat.go` 仅在值非空时按该字段名发送上限（Agent 2048、工具闭环检测 256、聊天检测 64）。

## 验证

| 要求 | 证据 |
| --- | --- |
| 前台无标签与下拉框、键盘不可进入 | UI 脚本：no output-length control or label / no focusable element for the parameter；真实按键 Tab 顺序（390 px，高级设置展开）：超时 → Agent 连接方式 → 保存 → 诊断 → 聊天测试 |
| 新连接默认不发送 | UI 脚本：new connection sends no output-length parameter、`[max_tokens]`/`[max_completion_tokens]` new connection defaults to none；`agent.TestMaxTokensParamSurvivesPageActions`（备用连接保存为空） |
| 三种值在编辑、备用保存、检测使用、切换与切回、停用后保持 | UI 脚本每个值 7 项（`[empty]`、`[max_tokens]`、`[max_completion_tokens]`：summary / unchanged resubmit sends stored values / edit + check-and-use keeps value / backup save keeps the other connection's value / switch sends the target's value / switch back restores value / disable keeps value）；Go 子测试 `param=`、`param=max_tokens`、`param=max_completion_tokens` 对真实运行时与数据库做同样流程 |
| 按原字段名发送原上限 | `modelprovider.TestMaxTokensParamWireField`（假 HTTP 服务器检查请求体：空值两字段都不出现；其余只出现所选字段，值等于请求上限）；Go 子测试检查检测与运行时客户端拿到的参数名 |
| 已生效配置原样提交仍为 unchanged | Go 子测试：原样提交 `Unchanged=true`，当前配置版本不变，不发起检测；UI 脚本确认页面原样提交的字段与已存值完全一致 |
| 原 UI 回归与 MC-01～MC-05 | `model-ui-check.js` 共 63 项（原 37 项 + 本次 26 项），1280、375、390 px 全部通过、无横向溢出；`TestAdminChangeDuringCheckRejectsActivation`、`TestKeyOnlyChangeIsPendingUntilApplied` 等原测试通过 |

执行的命令：`make check`（`go test -race ./...`、`go vet`、gofmt、staticcheck）；`go test -count=1 -race ./internal/agent/ ./internal/admin/ ./internal/modelprovider/ ./internal/store/`；`CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto`；`git diff --check`；`docker compose config --quiet`。

## 验证边界

以上均使用假模型、假 HTTP 服务器或假 API，不代表真实供应商验证。真实模型列表获取与双连接切换仍需生产管理员登录及第二个可用模型凭据；真实分类写入需要专用测试邮件；未具备时在发布报告中标为未验证，不以健康检查或 `noop` 运行代替。

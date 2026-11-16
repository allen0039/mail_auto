# C1 隔离 QA 实际重启与只读连接核验

日期：2026-11-17，时间均为 Asia/Shanghai。供主代理独立复核，不构成整体验收或发布批准。

## 候选与前置安全

- HEAD `31478997b06caab26b43f7010edc5448e54f24b6`；产品候选 `ac29317395f21cf12a9b006e2adc2f4eec3eea60`。`git diff ac29317 HEAD -- cmd internal go.mod go.sum` 为空，未修改产品源码。
- 本地执行 `go build -o .planning/checkpoint-c1/mail-auto ./cmd/mail-auto`，退出0；version=0.10.4、schema15。二进制SHA256 `f3220fdda3fc3815ebeb7dddbac96ffa117969111816adcca3c21a9636a2c8e5`，构建信息在证据 `build-info.txt`。
- 停机前确认18090无监听、数据库无其他持有者、queued/running=0；24 completed / 3 needs_verification；两个账号写入关闭、任务启用数0。
- SQLite backup API制作仓库外一致数据库备份并复制配套主密钥；完整性ok、密钥一致，备份目录0700、文件0600，没有输出或提交密钥。
- 审查StartOperations/finishRecoveredMove/Gmail FinishMove：正常启动仍会后台只读核验unknown；远端清理由写入关闭保护拒绝，Gmail补偿入口另行拒绝。本轮不手动Reconcile或结案。

## 真实执行结果

| 事件 | 结果 |
| --- | --- |
| 首次启动 | 01:34:11，PID85130；readyz=ready/0.10.4/schema15 |
| 首次只读诊断 | 01:34:49–01:34:53，Google/Microsoft官方OAuth IMAPS与LIST均成功，19/13目录 |
| 正常停止 | SIGTERM后确认18090和数据库释放、旧进程退出；精确信号时间未单独记录，仅能界定01:34:53之后、01:35:23之前 |
| 实际再次启动 | 01:35:53，PID85404，同一二进制/参数；readyz=ready/0.10.4/schema15 |
| 重启后只读诊断 | 01:36:45–01:36:50，Google/Microsoft均成功，19/13目录 |
| 最终安全核验 | 24 completed / 3 needs_verification、queued/running=0；三份unknown冻结计划摘要不变；账号主体/代际/应用版本/scope及文件夹范围摘要不变；两个账号connected；写入与任务继续关闭 |
| 入口 | https://localhost:18444，使用既有私有Caddy CA验证readyz退出0；18090唯一监听PID85404，18444既有OrbStack代理 |

启动显式使用 `--automatic-sync=false --mcp-enabled=false --secure-cookie`，原数据/密钥目录及origin不变。没有Sync、造邮件、修改标签、MOVE/COPY/STORE/EXPUNGE、手动核验/结案或扩大Gmail四QA标签范围。正常后台核验时间字段可能变化，不声称全部数据库字节不变。

连接诊断通过现有Service.DiagnoseNetwork和配置Network.Manager，使用既有OAuthManager刷新/CAS写回机制，不自建token交换、不绕代理。诊断helper未启动worker；fresh helper不是服务重启证据，也不证明服务内token缓存已预热。仅连接/LIST，不新增EXAMINE、同步、整理或网页对照证据。目录名称与私人正文未保存。

## 异常、计数与证据

首次停机快照遇Python SQLite `unable to open database file`；当时WAL sidecar不存在。系统sqlite3执行 `PRAGMA journal_mode; PRAGMA integrity_check` 返回wal/ok后，停机快照重试通过。没有业务SQL写入；底层原因未确证，不掩盖失败。

该停止脚本失败后，同一shell的后续诊断仍执行，因此 `second-diagnostics.json` 时间01:35:23–01:35:27，早于第二次服务启动，**不能当作重启后证据**。真实重启后证据为 `post-restart-diagnostics.json`；异常与时间精度说明保留在 `execution-caveats.json`。

证据均在 `.planning/checkpoint-c1/`，不入Git：before/first-start/first-start-safe/first-diagnostics/first-diagnosed-safe/stopped-safe/restart/restarted-safe/post-restart-diagnostics/post-restart-final-safe JSON，构建信息及helper源码。两轮正式诊断4次成功/0失败；中间额外诊断两家成功不重复计入。安全快照重试通过；首次工具失败单列。未重跑B全量门禁；本轮构建、诊断、最终安全断言及git diff --check退出0。

## 保留状态与待办

唯一QA实例继续运行，写入/任务/自动同步关闭。本人可到本机受保护管理页原账号卡片按后续验收要求选择“重新授权”，自己选择原邮箱并同意官方权限；本轮未提前生成state、不代点登录/同意，不需向聊天提供任何凭据。

C1第5项允许可选的官方Outlook专用QA目录只读检查；本轮未尝试该项，不推断现有登录故障或当前可用性，网页对照仍待验。三个Gmail unknown仍未结案，剩余真实整理、重授权等矩阵未宣布完成。

## 主代理独立复核反馈

主代理随后反馈已独立确认：18090唯一监听PID85404，执行路径为checkpoint-c1/mail-auto，二进制SHA256与本报告一致；后端及HTTPS localhost:18444/readyz均为ready/0.10.4/schema15。主代理以活动数据库mode=ro核对两个主体、代际、scope、Gmail四标签范围和三个unknown冻结摘要均与before一致，24completed/3unknown，写入及任务关闭。

主代理已审阅first/post-restart两轮helper诊断结果。诊断由实施者的独立helper调用Service路径发起，不是主代理亲自发起，也不是运行服务的HTTP诊断；此归属不因材料复核而改变。SQLite重试、第二份诊断早于实际重启、停机信号仅知时间区间等限制继续保留，不倒填数据。

主代理复核时发现私有 `runtime.json` 仍保留旧PID 81359和旧私有bin路径。已检查仓库、LaunchAgents/LaunchDaemons及本机脚本引用，未发现该文件被现有launcher或stop脚本引用；它是运行元数据而非当前服务的权威进程锁。为避免后续人工/脚本误判，已在不触碰origin、数据/密钥参数或凭据的前提下原子更新该文件：`pid=85404`、`version=0.10.4`、`binary_path`及command首项指向当前 `.planning/checkpoint-c1/mail-auto`。文件权限仍为0600。当前权威状态以实际PID、18090监听、readyz和 `restart.json`/`post-restart-final-safe.json`为准。

C1脱敏交接完成，保持当前唯一QA实例运行，不再重启或重跑B。Outlook官方网页QA目录对照尚未执行；本轮未尝试浏览器，不能推断现有登录是否可用，也不能把该缺项表述为已验证的登录失败。后续如需登录，由本人在官方页面完成；不反复浏览私人收件箱。重新授权及其余真实验收仍待后续安排。

**C1运行状态已获主代理上述独立核对；真实整体验收仍未完成，尚未发布部署。** 收尾按主代理授权仅提交本报告及原handoff顶部C1链接两份脱敏文档；未推送、发布tag、上传镜像、部署Oracle或清理资源。

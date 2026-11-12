# 0.4.19 验证范围

日期：2026-11-12。本文记录 0.4.19 的缺陷修复和本地模拟验证。数据库保持 schema 9，无迁移。实际标签、镜像摘要、部署和生产验证结果以 GitHub [0.4.19 发布记录](https://github.com/allen0039/mail_auto/releases/tag/v0.4.19)为准；本文的本地与模拟检查不能替代生产验证。

## 一、缺陷 R19-01：系统取消后从未尝试的邮件被永久去重

| 项目 | 内容 |
| --- | --- |
| 发现方式 | 隔离审计（本地模拟），不是线上故障。 |
| 现象 | 任务期限到期（`task_timeout`）或工作租约过期（`worker_lease_expired`）时，系统取消已提交但写入 worker 尚未启动的操作，操作项结束为 `cancelled`，未发出任何 IMAP 写入。之后同一任务版本的下一轮领取把该邮件判为已处理，返回 `items=0 / state=empty`，邮件不会再被自动整理。 |
| 原因 | `claimAvailable` 只要看到原计划的操作已结束，且邮件指纹没变，就拒绝领取，并没有区分“已写入”和“系统取消、从未尝试”。 |
| 修复 | 仅当原操作 `cancelled=1`、`cancel_source` 正好是 `system:task_timeout` 或 `system:worker_lease_expired`，且该邮件在原操作中的所有操作项都是 `cancelled`、没有目标 UIDVALIDITY/UID 映射时，才允许下一轮重新领取。操作仍为 `queued`/`running`/`needs_verification` 时继续阻止领取。 |
| 不变项 | 手动停止（`user`）、空停止来源、其他 `system:*` 原因，以及 completed/failed/skipped/running/needs_verification 的项目，仍保持原有的不重复领取行为。旧计划和旧 MOVE 不会重放，也不删除任何记录；新计划完整校验当前规则、授权、目录映射、保护和任务版本。每批上限、未读/星标保护、日程、规则含义和网络超时都不变，也没有新增每日额度。 |

## 二、本地验证

| 检查 | 结果 |
| --- | --- |
| 修复前，在 0.4.18 源码上运行 Codex 独立探针 `TestProbeSystemCancelledUnattemptedClaimIsRetriable` | 两个子场景都失败：`items=0 state=empty`，缺陷复现。 |
| 修复后，用同一个 overlay 运行该探针（探针未修改） | `task_timeout`、`worker_lease_expired` 均 PASS。 |
| `TestSystemCancelledUnattemptedItemIsReplannedOnce` | 两种触发路径：已提交、写入未开始 → 系统取消 → 操作和操作项为 `cancelled`，`cancel_source=system:<原因>` → 同一 `run_key` 仍返回原运行 → 下一轮相同版本可以领取并生成新计划（plan_id 不同）→ 只移动 1 次；再次运行旧操作不会重放，下一轮不再领取。 |
| `TestSystemCancelledClaimRetryNegativeCases` | 空来源、`system:task_failed`、`system:`、有目标映射（完整、只有 UID、只有 UIDVALIDITY）、操作项为 completed/failed/skipped/running/needs_verification、操作为 queued/running/needs_verification、未标记 cancelled：都不能重新领取，且没有写入。 |
| `TestUserCancelledClaimIsNotOverriddenBySystemCancel` | 先手动停止，再触发两种系统取消：`cancel_source` 保持 `user`，邮件不会重新领取。 |
| `TestSystemCancelledRetryIsJudgedPerItem` | 同一批次的两封邮件都因系统租约过期而取消，其中一封的操作项被设成已完成并带映射，另一封未尝试。下一轮只重新领取未尝试的那封。 |
| `TestSystemCancelledRetryRequiresEveryItemForRef` | 同一邮件在原操作中还有一个已完成、带映射的项目时，不能重新领取。 |
| `go vet ./...`、`go test ./... -count=1`、`gofmt -l` | 全部通过，无格式差异。 |

说明：自动化计划在执行时要求单个计划只含一个项目，因此逐项判断由“同一批次多个操作”和“同一操作中同一 ref 有多个项目”这两项回归覆盖。

## 三、真实邮箱对照（0.4.18，不是 0.4.19 的验证）

0.4.18 生产环境本轮分类 6 封全部完成，途中 1 项经自动核验恢复。6 项独立只读 IMAP 检查（源、目标、指纹、标记）均通过；清理任务没有符合条件的候选。R19-01 来自隔离审计，不能写成这 6 封失败。

## 四、未验证与已知限制

- 本文不记录生产验证；0.4.19 在生产环境的版本、健康检查、邮箱连接、文件夹同步和分类任务结果见 GitHub 0.4.19 发布记录。
- 本版不解决网易等服务商的网络波动；网络超时和未知写入的恢复语义不变。
- 历史上有 1 项没有可信 COPYUID，仍保持待核验，不会根据相似元数据自动删除源邮件。

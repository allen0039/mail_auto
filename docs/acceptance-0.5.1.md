# 0.5.1 验证范围

日期：2026-11-13（Asia/Shanghai）。0.5.1 修复独立审计报告的 AT-AUD-01～08，数据库升级到 schema 11（只新增列）。本文记录本地离线验证；全部使用临时 SQLite、假模型、假 Telegram 与假邮箱，**不能替代真实模型、真实 Bot、专用测试邮件和生产快照回退的验证**，也不是独立审计结论。

## 修复与对应测试

| 编号 | 修复 | 回归测试 |
| --- | --- | --- |
| AT-AUD-01 | 中断轮次的已完成写入用单条关联查询读取，避免单连接自锁 | `agent.TestStartRecoversCommittedDraftWithoutBlocking`；审计探针 `TestAuditRecoveryAfterCompletedDraftDoesNotBlock` |
| AT-AUD-02 | 回复冻结授权版本；授权变更事务内推进上下文、失效方案与轮次、抑制旧回复；限流等待后复核实时授权；命令结果生成前冻结授权快照 | `telegram.TestOutboxFencesForEveryGrantChange`、`TestSplittingPlainText429AndUnknownDelivery`（无版本回复不外发）、`agent.TestGrantSaveAdvancesContextImmediately`；审计探针 `TestAuditGrantShrinkSuppressesOldReply`、复审探针 `TestReauditGrantNaturalExpirySuppressesQueuedReply`、`TestReauditCommandContentCannotAcquireNewGrantVersion`；`/new` 回执（无邮箱数据）采用重置后的上下文、授权版本仍冻结：`telegram.TestNewSessionAcknowledgementIsDelivered`、复审探针 `TestReauditNewSessionAcknowledgementSurvivesItsOwnReset` |
| AT-AUD-03 | 停用 Agent/修改配置在同一事务中取消轮次、失效 pending/accepted/applying 方案；方案冻结配置版本 | `agent.TestDisableBetweenAcceptAndApplyAndReenableDoesNotRevive`、`TestApplyingStopsAtNextStepForEachLifecycleChange/agent_disabled`；审计探针 `TestAuditDisabledProviderRejectsOldConfirmation`、复审探针 `TestReauditProviderDisableFencesAcceptedAndApplying` |
| AT-AUD-04 | 每个方案步骤在业务事务内运行围栏（线性化点） | `agent.TestApplyingStopsAtNextStepForEachLifecycleChange`（收窄/撤销/到期/管理员变化/停用/停止/解绑 + 重启）、`TestGuardLinearizesStepAgainstGrantChange`（受控屏障）；审计探针 `TestAuditApplyingProposalRejectsChangedGrant` |
| AT-AUD-05 | 模型 `run_task` 只生成确认卡片；确定性 `/run <task_id>` | `agent.TestModelRunRequestsNeverQueueWithoutConfirmation`、`TestRunCommandSemantics`、`TestRunTaskLinksRealRunAndReportsVerifiedResult`（确认后经真实执行路径完成 2 封核验移动）、`telegram.TestEndToEndTelegramAgentWorkflow`、`TestRunCommandMustBeFresh`（超过 10 分钟的 `/run` 不执行）；审计探针 `TestAuditReadOnlyRequestCannotStartMailTask`、复审探针 `TestReauditExpiredRunCommandRequiresFreshIntent` |
| AT-AUD-06 | 控制事件在接收时标记并优先；`/stop` 丢弃其前的待处理普通消息 | `telegram.TestControlPriorityOnlyForBoundSenderAndStopDropsEarlierWork`；审计探针 `TestAuditQueueFullDoesNotBlockStop`、复审探针 `TestReauditStopCancelsOlderUnsubmittedInbox` |
| AT-AUD-07 | 同 Bot 重开时把有效绑定同步到新接收代际 | `telegram.TestReenableRefreshesBindingWithoutRevivingOldWork`、`TestCredentialChangeRevokesBindingAndFencesOldWork`；审计探针 `TestAuditReenableKeepsRepliesUsable` |
| AT-AUD-08 | 页面与 API 要求明确文件夹范围（`all_folders`） | `admin.TestAgentGrantFolderScopeIsExplicit`；审计探针 `repro-ui.cjs`；浏览器表单验证 |
| 迁移 | schema 10→11 只加列，关闭旧版本未完成方案和未发送回复，可重复打开 | `store.TestSchema10To11ClosesOldAgentWork`、`TestSchema9UpgradeKeepsDataAndStartsDisabled`、`TestSchema10MigrationFailureRollsBack` |

0.5.0 的 T01–T28 对应测试见 [0.5.0 验证范围](acceptance-0.5.0.md)，仍全部运行；其中涉及执行入口的测试已按“模型请求执行需确认卡片”调整，执行结果断言保留。

## 与原方案的差异

- 自然语言执行一律走确认卡片；不经确认的快速执行只有确定性命令 `/run`，且仅限已启用任务。
- 同 Bot 重新开启交互保留绑定（刷新接收代际），不强制重新绑定。
- 任何 Agent 模型配置保存都会使未完成方案失效（保守）。

## 仍未验证

真实 OpenAI 兼容模型的检测与对话、真实 Telegram Bot 的绑定与卡片、经 Agent 在专用测试邮件上的实际移动、生产快照回退演练。

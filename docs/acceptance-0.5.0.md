# 0.5.0 验证范围

日期：2026-11-13（Asia/Shanghai）。本文记录 0.5.0（内置管理 Agent 与 Telegram 交互，schema 10）的实现范围与本地离线验证。实际标签、镜像摘要、Oracle1 部署和生产验证结果以 GitHub [0.5.0 发布记录](https://github.com/allen0039/mail_auto/releases/tag/v0.5.0)为准。本文的离线测试使用假 Provider、假 Telegram 与假邮箱，不能替代真实模型、真实 Bot 与真实邮箱的验证，也不代表独立审计结论。

## 范围

实现依据 [实施方案](agent-telegram-implementation-plan.md) 的首版范围：OpenAI 兼容 Chat Completions Provider 与能力检测；独立可撤销的 Agent 授权与身份校验；18 个首版工具；草稿、只读草稿工作流预览、确认卡片、业务 mutation 幂等与多步骤恢复；Telegram 绑定、单消费者长轮询、持久 inbox、命令、回调与回复 outbox；Web「Agent 与 TG」页面；schema 10 迁移与保留期清理。使用方法见 [Agent 与 Telegram 指南](agent-telegram.md)。

未纳入首版：Responses 等其他供应商协议、持续 AI 语义分类与内置 AI 定时 worker、Web 完整聊天、多管理员、群聊、语音/图片/附件、任意 shell/SQL/HTTP/第三方 MCP 工具、发信、永久删除、自动建文件夹。

## 本地离线验证

| 检查 | 结果 |
| --- | --- |
| `make check`（`go test -race ./...`、`go vet`、gofmt、staticcheck v0.8.2） | 通过 |
| `govulncheck@v1.9.0 ./...` | 无可达的已知漏洞（详见发布记录） |
| `make build`、`docker compose config --quiet`、`git diff --check` | 通过 |
| 浏览器（本地临时实例）：「Agent 与 TG」页面加载无控制台错误；拒绝元数据地址；保存后显示最终请求地址且不回显 Key；检测失败显示固定错误码；未通过工具闭环时拒绝启用；无 Bot Token 时拒绝开启交互；375px 宽度无横向溢出 | 通过 |

## 故障矩阵对应测试

| 编号 | 测试（包内函数名） |
| --- | --- |
| T01 | `modelprovider.TestProbeDistinguishesChatFromToolLoop`，`agent.TestProviderCheckBoundToConfiguration` |
| T02 | `modelprovider.TestDecodeRejectsPartialOrInvalidToolCalls`，`mailtools.TestRegistryWhitelistAndStrictArguments`，`agent.TestModelCannotForgeIdentityOrConfirm` |
| T03 | `modelprovider.TestProviderErrorsRetriesAndNoLeak` |
| T04 | `modelprovider.TestDestinationGuard`，`admin.TestAgentAPIBoundaryAndRedaction` |
| T05 | `agent.TestProviderCheckBoundToConfiguration`，`agent.TestInFlightCheckCannotMarkNewConfigReadyAndQueueIsBounded` |
| T06 | `agent.TestModelCannotForgeIdentityOrConfirm`，`mail.TestAgentIdentityNeedsPersistedGrantAndBinding` |
| T07 | `mail.TestAgentIdentityNeedsPersistedGrantAndBinding`，`mail.TestAgentGrantExpiryAndInvalidInput`，`agent.TestProposalFencedBySupersedeStateChangeAndRevocation`，`agent.TestGrantChangeDropsOldContext`，`mail.TestAgentOwnedRunExecutesAndIsFencedByGrant` |
| T08 | `mail.TestAgentCrossOwnerRunsAreScopedAndMCPIsolationUnchanged` |
| T09 | `mail.TestDraftWorkflowPreviewDoesNotEnableAndIncludesClosure`，`mailtools.TestWorkflowProposalAppliesInOrderAndIsIdempotent`，`mailtools.TestDraftsNeverEnableAndReadOnlyGrantCannotWrite`，`mail.TestAgentCannotStartAIRuns` |
| T10 | `mailtools.TestTaskPatchKeepsUnspecifiedFieldsAndNoDailyQuota` |
| T11 | `mailtools.TestWorkflowProposalAppliesInOrderAndIsIdempotent`（本次扫描与来源总数分开、0 候选说明） |
| T12 | `agent.TestProposalFencedBySupersedeStateChangeAndRevocation`，`telegram.TestCommandsWorkWithoutModel`（普通文本在接收时提升意图代际） |
| T13 | `mail.TestMutationIdempotencyForRulesAndTasks`，`agent.TestWorkflowDraftProposalConfirmEndToEnd`，`telegram.TestEndToEndTelegramAgentWorkflow` |
| T14 | `agent.TestRestartInterruptsTurnAndResumesAcceptedProposalOnce`，`agent.TestRegeneratedCallReusesActionAndLimitsStopLoops`，`mailtools.TestWorkflowProposalAppliesInOrderAndIsIdempotent` |
| T15 | `mailtools.TestPartialWorkflowStopsOnConflictWithoutRollback` |
| T16 | `telegram.TestOffsetAdvancesOnlyAfterWholeBatchPersists` |
| T17 | `telegram.TestGapsBacklogPoisonAndOldText` |
| T18 | `telegram.TestWebhookConflictAndLeaseStopPolling` |
| T19 | `telegram.TestCredentialChangeRevokesBindingAndFencesOldWork` |
| T20 | `telegram.TestBindingRejectsGroupsForwardsSpoofingAndReuse` |
| T21 | `telegram.TestCallbacksAnsweredAndVerifiedBySender`，`agent.TestWorkflowDraftProposalConfirmEndToEnd` |
| T22 | `telegram.TestSplittingPlainText429AndUnknownDelivery`，`tglimit.TestBlockAppliesToRepliesAndPacingToBoth` |
| T23 | `agent.TestStopCancelsHungModelAndQueuedWork`，`telegram.TestCommandsWorkWithoutModel` |
| T24 | `mail.TestAgentCrossOwnerRunsAreScopedAndMCPIsolationUnchanged`，既有 `mail.TestCancelledTaskRetainsUnknownMailOutcome` |
| T25 | `agent.TestRegeneratedCallReusesActionAndLimitsStopLoops`，`agent.TestInFlightCheckCannotMarkNewConfigReadyAndQueueIsBounded` |
| T26 | `store.TestSchema9UpgradeKeepsDataAndStartsDisabled`，`store.TestSchema10MigrationFailureRollsBack`，既有密钥与新 schema 拒绝测试 |
| T27 | `store.TestAgentRetentionKeepsActiveRecords` |
| T28 | 全量既有测试（Web、MCP、同步、恢复、通知）；`agent.TestRunTaskLinksRealRunAndReportsVerifiedResult` 经真实任务执行路径在假邮箱完成 2 封核验移动 |

## 已知限制

- 只实现 Chat Completions；`strict`、`parallel_tool_calls` 不自动探测（不发送 strict，工具参数始终由应用端严格校验）；输出长度参数由管理员选择。
- 模型地址只允许 HTTPS 公网目的地；使用本地模型或代理需要后续版本的显式白名单。
- 持续 AI 分类、内置 AI 定时 worker 未开放；Agent 不创建、不修改、不触发 AI 引擎任务，也不停止其他 owner 的 AI 批次。
- Telegram 服务端最多保留 24 小时更新；离线超过 24 小时的消息无法补齐。超过 10 分钟未处理的普通文本只提示重发。
- 消息投递可能重复（发送超时后结果未知会再试一次），但不会重做业务写入。
- 回退必须恢复升级前快照与配套主密钥；回退不撤销已经发生的原邮箱移动。
- 首版不做进度消息的原地编辑，只推送运行开始与结束；同一聊天已有任务通知时不重复推送最终结果。
- 已验证的邮箱提供商范围不扩大：QQ、iCloud 仍待专用测试邮件验证。

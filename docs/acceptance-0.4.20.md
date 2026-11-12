# 0.4.20 验证范围

日期：2026-11-12。本文记录 0.4.20 的缺陷修复和本地模拟验证。数据库保持 schema 9，无迁移。实际标签、镜像摘要、部署和生产验证结果以 GitHub [0.4.20 发布记录](https://github.com/allen0039/mail_auto/releases/tag/v0.4.20)为准；本文的本地与模拟检查不能替代生产验证，也不表示生产环境的文件夹同步已经恢复。

## 一、缺陷 R19-02：只读核验使进行中的自动同步反复失效

| 项目 | 内容 |
| --- | --- |
| 发现方式 | 0.4.19 生产验收中观察到现象，随后用本地模拟独立复现。 |
| 生产现象（0.4.19） | 2026-11-12T14:02:20Z 启动后，分类复跑扫描 264、清理复跑扫描 500，均无领取、无失败；但 14 个文件夹超过 14 分钟没有任何同步更新。日志中待核验移动的自动核验约每 30 秒一次，并间歇出现服务商 45 秒读取超时。 |
| 原因 | 核验获取账号锁时使用与前台请求相同的 `acquire`，每次都推进该账号的同步版本号（epoch）。自动同步在提交前比对 epoch，于是只要核验在同步读取期间运行一次，已读取的快照就作废并返回 `ErrBusy`；慢速网络让同步窗口变长，与 30 秒核验周期叠加后形成活锁。属于代码活锁，网络慢只是触发条件；未发现永久占用锁的泄漏。 |
| 修复 | 新增 `acquireRecovery`：占用同一账号独占锁和共享的两个读取名额，但不推进 epoch。核验在发布任何移动后的索引行之前（VerifyMove 确认后）、以及调用 FinishMove 尝试源邮件清理之前，在持有锁时显式推进 epoch；清理失败或结果未知同样已推进。只读结论（VerifyMove 未确认后被前置条件阻止、没有目标 UID 的 InspectMove、InspectMove 出错、只把操作项记为 `move_not_applied` 的“源邮件仍在”）不推进 epoch。 |
| 不变项 | 核验占用期间自动同步仍不能提交（账号独占检查不变）；手动请求、配置修改、手动同步和任务批次仍推进 epoch。没有可信目标 UID 时保持待核验，不凭元数据确认、不重放 MOVE。任务、规则、每批上限、日程、未读/星标保护、IMAP 超时、核验时间窗和每日额度（无）都不变。 |

## 二、本地验证

| 检查 | 结果 |
| --- | --- |
| 修复前，在 0.4.19 源码上运行 Codex 独立探针 `TestProbeReadOnlyUnknownRecoveryPreservesInflightSync`（overlay，未纳入源码） | 3 次刷新均返回 `ErrBusy`，标记和索引无变化，缺陷复现。 |
| `TestReadOnlyUnknownRecoveryKeepsInflightFlagRefresh` | 无目标 UID、目标目录有元数据相似副本，以及 InspectMove 出错两种情形：同步读取期间连续 3 次核验，每次都保持待核验、`recovery_no_trusted_mapping`，MOVE 仍只有 1 次、无清理，epoch 和索引不变；3 次只标记变化的刷新全部发布。 |
| `TestSourcePresentRecoveryKeepsInflightFlagRefresh` | “源邮件仍在”只更新操作记录为 `move_not_applied`，进行中的刷新照常发布。 |
| `TestVerifiedRecoveryPublishInvalidatesInflightRefresh` | 有目标 UID 且 VerifyMove 确认：核验发布移动后 epoch 已推进，旧快照返回 `ErrBusy`，索引保持核验发布的目标位置。 |
| `TestRecoveryCleanupAttemptInvalidatesInflightRefresh` | 源清理结果未知、失败、成功三种情形：FinishMove 各调用 1 次、MOVE 仍 1 次，epoch 都已推进，旧快照均被拒绝。 |
| `TestBlockedCleanupWithoutRemoteWriteKeepsInflightRefresh` | 原邮箱写入已关闭，清理在任何远端写入前被拒绝：保持待核验，不推进 epoch，刷新照常发布。 |
| `TestRecoveryGateKeepsAccountExclusionAndReadBudget` | 核验占用期间：前台请求、第二个核验、手动同步都返回忙，自动同步提交被拒绝；释放后刷新可正常发布。两个读取名额被占满后，核验和前台请求都返回忙。 |
| 反向检查 | 临时改回 `acquire` 时，三项只读用例失败；临时去掉两处显式失效时，发布与清理两项用例失败。恢复修复后全部通过。 |
| 已有回归（未修改） | `TestIncrementalRejectsResultsAfterConfigAndManualRequest`（手动请求、配置修改仍使同步失效）、0.4.18/0.4.19 的核验、清理、取消、映射与 R19-01 回归全部通过。 |

## 三、未验证与已知限制

- 本文不记录生产验证；0.4.20 部署后的版本、健康检查、邮箱连接、文件夹同步和分类任务结果见 GitHub 0.4.20 发布记录。生产文件夹同步是否恢复，必须以部署后的实际同步记录为准。
- 本版不解决网易等服务商的网络波动。核验占用账号期间（例如遇到 45 秒读取超时时），自动同步仍需等待并在释放后重试；待核验项目很多且核验持续占用账号时，同步仍可能明显变慢。
- 没有可信 COPYUID 的历史待核验项目仍保持待核验，需要人工核对，不会根据相似元数据自动删除源邮件。

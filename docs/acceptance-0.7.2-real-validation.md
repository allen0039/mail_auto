# 0.7.2 真实业务补充验证

状态：**验证结束，待 Codex 独立复验**。验证时间 2026-11-14T13:55Z～15:14Z。未经 Codex 复验，不代表验收通过。

## 总体结论

| 项目 | 结论 | 说明 |
| --- | --- | --- |
| 真实模型列表获取 | 通过 | 真实页面点击“获取模型”，连接 A 的供应商返回 28 个模型；当前生效配置未改变 |
| 两个真实连接切换及切回 | 通过（附说明） | A→B（管理员操作）与 B→A（执行者操作）均为真实工具闭环检测成功，B、A 各完成一次真实聊天测试；页面、API、数据库一致。说明：A、B 为同一服务域名，差异为接口路径与模型，见下文 |
| 真实分类写入 | 受阻（未验证） | 管理员决定本轮不提供专用测试邮件；未执行任何写入。不以 noop 任务或模拟结果替代 |

## 环境与基准（2026-11-14T13:55:25Z 采集，只读）

- 源码提交：`89a7f6c16bafeb6dce3ba6395bab87f71b315418`（`v0.7.2`）。
- Oracle1 容器 `mail-auto`：ImageID `sha256:e9fef60599dc67d8bd24d6a94d403905a4cff668ae41275791767e2cd1d17180`，RepoDigest `allen0039/mail-auto@sha256:feae7d60d8411f520a8761e71aec6ac3a1241fc71fc1c120b6a0f4cf3a50dabf`；healthy，重启 0，启动于 2026-11-14T12:46:17Z。
- `/readyz`：`{"schema":13,"status":"ready","version":"0.7.2"}`。
- 恢复基准（连接 A）：

| 字段 | 值 |
| --- | --- |
| 当前配置版本 | 2，启用 |
| 当前连接 | `legacy-ciDA0hWHfYJ3ercx`（名称 magpie.mfallen.de，连接版本 1） |
| 模型 | `auto/gpt-6.1-terra` |
| 超时 / max_tokens_param | 60 秒 / 空 |
| 工具闭环检测 | ok，检测时间 1794551911（2026-11-13T06:38:31Z） |
| 密钥 | 已配置（不回显） |

- 进行中工作：`agent_turns` 仅 1 条 completed（2026-11-13）；`agent_proposals` 0 条；无待确认方案。切换连接不会使正常工作失效。
- Agent 授权：1 个账户，动作 `read, rule_manage, task_manage, automation, task_run_manage`，有效至 2026-12-13。Telegram 交互 online，绑定 1 个（active）。

## 写入路径（代码核对，未执行）

0.7.2 中由真实模型驱动的邮箱写入只有一条路径：管理员在 Telegram 向机器人发消息 → Agent 用真实模型读取邮件并调用工具 → 生成待确认方案（新建/修改规则与任务，或运行任务）→ 管理员在 Telegram 点“确认执行” → 规则任务在服务器端执行移动。内置定时任务只按规则执行，需 AI 判断的邮件保留原位（`needs_ai`）。模型不直接移动单封邮件；授权中没有单封移动动作。因此真实分类写入的证据链为：模型调用记录（turn / tool calls）→ 方案与确认 → 任务运行 ID 与写入结果 → 服务器端文件夹变化。

## 仍缺条件

1. 专用测试邮件（唯一主题、所在文件夹、目标文件夹、恢复约定）。
2. 写入验证期间管理员在 Telegram 发消息并确认方案。

## 登录方式

管理员在本人 Chrome 中已登录；执行者通过 Claude in Chrome 在新标签页操作真实页面，未输入密码或密钥。连接名称等字段在扩展输出中可能被自动遮蔽，以连接 ID 记录。

## 三项验证记录

### 1. 真实模型列表获取 —— 通过

| 项目 | 记录 |
| --- | --- |
| 时间 | 2026-11-14T14:39:2x Z 点击，14:39:31Z 读取结果 |
| 连接 | `legacy-ciDA0hWHfYJ3ercx`（A），服务商域名 `magpie.mfallen.de`，使用已保存密钥（同源，未重新输入） |
| 操作 | 「Agent 与 TG」→ 模型连接 → 点击“获取模型”（真实鼠标点击） |
| 页面结果 | “已获取 28 个模型：在“模型”框中输入关键字搜索，或直接填写。” |
| 返回数量 / 示例 | 28；`auto/gpt-6-astra`、`auto/gpt-6-luna`、`auto/gpt-6.1-sol`、`auto/gpt-6.1-terra`、`codex/gpt-5.6-luna`、`codex/gpt-5.6-sol` |
| 进入候选列表 | `#model-options` 含 28 项，包含当前模型 `auto/gpt-6.1-terra`；在模型框输入关键字会出现候选，可直接填写 |
| 当前配置不变 | 获取后 API：当前配置版本 2、连接 `legacy-ciDA0hWHfYJ3ercx`、模型 `auto/gpt-6.1-terra`、连接版本 1；服务器数据库同值 |
| 审计 | 获取模型列表不写审计（设计如此，仅读取） |

**操作偏差（如实记录）**：在模型框输入关键字 `luna` 后按方向键与回车，回车提交了表单，以不完整的模型名 `luna` 触发了一次“检测并使用”。真实供应商拒绝（`provider_request_rejected`），页面显示“检测未通过，当前生效配置未改变”。审计 1656 `agent.connection.check` / `tool_loop` / `provider_request_rejected`（14:39:50Z）。服务器核对：当前配置版本 2、连接版本 1、模型与检测时间均未变化。该事件意外证实了“真实检测失败时保持当前配置”的路径，但不是计划内操作；随后点击“取消修改”恢复表单。

### 2. 两个真实连接切换及切回 —— 通过（附说明）

连接差异（均为真实可用连接，各自独立加密保存密钥，执行者未输入密钥）：

| | A `legacy-ciDA0hWHfYJ3ercx` | B `ydYEiCmSCBsviPWo` |
| --- | --- | --- |
| 服务域名 | magpie.mfallen.de | magpie.mfallen.de |
| API 地址 / 请求地址 | `https://magpie.mfallen.de/v1` → `/v1/chat/completions` | `https://magpie.mfallen.de` → `/chat/completions` |
| 模型 | `auto/gpt-6-luna` | `auto/gpt-6.1-terra` |
| 超时 / max_tokens_param | 60 / 空 | 60 / 空 |
| 创建 | 2026-11-13（0.7.0 迁移） | 2026-11-14T15:11:28Z（管理员在页面创建） |

A、B 不是不同供应商；两者接口路径与模型不同，不是同一配置的重复命名。是否使用同一把密钥执行者无法、也不应查看。

| 步骤 | 时间（UTC） | 操作者 | 结果 | 配置版本 | 审计 |
| --- | --- | --- | --- | --- | --- |
| A 原样“检测并使用” | 14:40:28 | 执行者 | unchanged（“该配置已在使用中，无需重新检测。”），**不计为新检测** | 2 | 无 |
| A 修改模型并检测使用 | 15:04:35 | 管理员 | 真实工具闭环 ok；A 模型 `auto/gpt-6.1-terra` → `auto/gpt-6-luna`（同一连接内） | 2→3 | 1661 check ok、1662 activate |
| 新建 B 并检测使用（A→B） | 15:11:28 | 管理员 | 真实工具闭环 ok，切换并启用 B（管理员直接使用“检测并使用”，未先保存为备用） | 3→4 | 1663 check ok、1664 activate |
| 一致性核对 | 15:12:05 | 执行者 | 页面当前生效“…auto/gpt-6.1-terra（检测于 23:11:28 北京时间）”，下拉 B 标“使用中”；API `connection_id`=B、`base_url`=`https://magpie.mfallen.de`；数据库同值；A、B 均 `pending=false` | 4 | — |
| B 聊天测试 | 15:12:22 | 执行者（真实点击） | “聊天测试通过（当前生效配置）。” `chat_at`=15:12:22 | 4 | 1665 provider.check chat ok |
| B→A | 15:12:44 | 执行者（选择 A，真实点击“检测并使用”，未填密钥） | “检测通过，已切换并启用（模型 auto/gpt-6-luna）。” 真实工具闭环 ok | 4→5 | 1666 check ok、1667 activate |
| A 聊天测试（确认运行时为 A） | 15:12:57 | 执行者（真实点击） | 聊天测试通过，`chat_at`=15:12:57；当前配置 `base_url`=`https://magpie.mfallen.de/v1`、模型 `auto/gpt-6-luna` | 5 | 1668 provider.check chat ok |

终态（15:13:02Z 数据库）：当前配置版本 5、启用、`connection_id`=A、工具闭环 ok（15:12:44）、聊天 ok（15:12:57）、`key_generation` 4；A 连接版本 2，B 连接版本 1；两者超时 60、`max_tokens_param` 空，切换前后未变。

密钥不串用：A、B 在各自激活后均 `pending=false`（服务端分别以各自 AAD 解密后与当前配置密钥比较一致）；页面密钥框始终为空并提示“已保存密钥（不回显）”；接口与审计输出不含密钥。

说明：A→B 一步由管理员完成，而非执行者；B→A 由执行者完成。两次都是本次发起的真实检测（审计与检测时间均为新值），没有以既有能力标记代替。

### 3. 真实分类写入 —— 受阻（未验证）

未执行。管理员在 15:13Z 后决定本轮不提供专用测试邮件，标记为受阻。没有任何邮件被移动、标记或改变状态；未修改任务、规则、每批数量、未读/星标保护或通知目标。

参考：同期内置定时任务 13:00、14:00、15:00 三轮为 `noop`（审计 1650–1655、1658–1660），不作为写入证据。15:08Z 核对：13:30Z 以来没有新邮件进入索引（最近同步 15:06:06Z）。

## 既有问题（非本次引入）

- 操作 `9vSORvT8…`（2026-11-12 创建，1 项）长期处于 `needs_verification`，约每 6 小时慢速核验一次仍未确认（审计 877、1472、1494、1513、1616、1636、1657）。需人工在邮箱中核对该项结果。
- 14:51–15:04Z 日志有 IMAP 超时、TLS 连接失败与重连警告；之后同步继续推进（15:06:06Z）。

## 恢复与清理

| 项目 | 结果 |
| --- | --- |
| 当前连接 | A `legacy-ciDA0hWHfYJ3ercx`，启用，工具闭环 ok（15:12:44Z）、聊天 ok（15:12:57Z） |
| 恢复基准变化 | A 的模型由管理员改为 `auto/gpt-6-luna`（15:04:35Z），管理员确认保留，因此未恢复为 `auto/gpt-6.1-terra` |
| 版本递增 | 当前配置 2→5、A 连接 1→2、`key_generation` 1→4，均因正常检测/切换产生，未回退 |
| 连接 B | `ydYEiCmSCBsviPWo` 由管理员创建，保留为备用（未生效）；执行者未删除，是否保留由管理员决定 |
| Agent 工作 | 无进行中 turn、无待确认方案（前后一致） |
| 任务与规则 | 4 个任务均启用、内置触发，下次运行时间正常；规则 10 条；数量与升级前清单一致；无相关审计变更 |
| 网络 | 本次未修改 Agent 连接方式或全局网络设置（无相关审计） |
| 浏览器 | 执行者在用户 Chrome 中新开的标签页已关闭；未修改登录状态 |

## Oracle1 复核（2026-11-14T15:14:32Z）

- 容器 `mail-auto`：ImageID `sha256:e9fef605…`（与 `0.7.2` arm64 一致），healthy，重启 0，启动时间未变（12:46:17Z）。
- `/readyz`：version 0.7.2、schema 13、ready。
- 14 个文件夹同步于 15:10:46～15:11:04Z；15:05Z 以来无 error 日志。
- 其他 23 个服务与升级前清单一致。
- 最近任务运行 C-RpBGXr 15:00:02Z `noop`（仅作调度正常参考）。

## 审计事件汇总

| ID | 时间（UTC） | 动作 | 结果 | 来源 |
| --- | --- | --- | --- | --- |
| 1656 | 14:39:50 | agent.connection.check | provider_request_rejected | 执行者误触（模型名 `luna`） |
| 1661 / 1662 | 15:04:35 | connection.check / activate | ok / enabled | 管理员修改 A 模型 |
| 1663 / 1664 | 15:11:28 | connection.check / activate | ok / enabled | 管理员 A→B |
| 1665 | 15:12:22 | provider.check chat | ok | 执行者 B 聊天测试 |
| 1666 / 1667 | 15:12:44 | connection.check / activate | ok / enabled | 执行者 B→A |
| 1668 | 15:12:57 | provider.check chat | ok | 执行者 A 聊天测试 |

## 供 Codex 复验

- 生产数据库（只读）：`/var/lib/docker/volumes/mail-auto_mail-data/_data/mail-auto.db`，查询 `audits WHERE id BETWEEN 1656 AND 1668`、`agent_provider_settings`、`agent_provider_connections`。
- 管理页面：`https://mailauto.mfallen.de` →「Agent 与 TG」→ 模型连接：可见 A（使用中）、B（备用），当前生效检测时间 2026/11/14 23:12:44（北京时间）。
- 复验模型列表获取：在 A 上点击“获取模型”，应返回约 28 个模型且当前配置版本不变（列表获取不写审计）。
- 发布回滚资源：`backups/upgrade-0.7.2-20261114-124614`、镜像 `allen0039/mail-auto:0.7.1`，本次未改动。

## 剩余问题

1. 真实分类写入未验证（受阻）。
2. 两个连接为同一服务域名（接口路径与模型不同）；跨供应商切换未验证。
3. A→B 由管理员操作完成；执行者完成了 B→A 与两次聊天测试。
4. 既有操作 `9vSORvT8…` 长期 `needs_verification`；IMAP 间歇超时重连。均非本次引入。
5. 本文件尚未提交到仓库。

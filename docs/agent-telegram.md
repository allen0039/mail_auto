# 内置管理 Agent 与 Telegram 交互（0.5.0）

管理员可在 Telegram 私聊中用自然语言创建和修改固定条件规则、试运行、创建定时任务、启用或暂停任务、按明确指令执行任务、查询进度并接收结果。模型采用可配置的 OpenAI 兼容 Chat Completions API。全部功能默认关闭。

持续 AI 语义分类、按正文判断是否需要回复、内置 AI 定时 worker 仍未支持。Agent 不会创建或触发 AI 引擎任务；收到这类需求时会说明限制并给出固定条件规则替代方案。

## 启用步骤

1. 「TG 通知」页面保存 Bot Token（交互复用同一个 Token；通知开关与交互开关相互独立，通知仍发到原通知 Chat）。
2. 「Agent 与 TG」→ 模型：填写 Base URL（API 根地址，例如 `https://api.example.com/v1`，页面显示最终请求地址 `…/v1/chat/completions`）、模型名称和 API Key，保存。
3. 依次点击「聊天测试」「工具闭环测试」。只有当前配置通过工具闭环测试，才能勾选「启用 Agent」保存。仅聊天通过时显示“可聊天，未验证工具调用”，不能启用。修改地址、模型、密钥或输出参数后检测自动失效。
4. 「Agent 授权」：选择账号、文件夹范围（建议“全部文件夹”）与动作，保存。默认只有“查询”。授权与管理员自动化策略取交集，不能扩大策略；执行任务时，授权须覆盖策略中的全部来源文件夹及目的文件夹，否则会明确拒绝并提示在 Web 调整。
5. 「Telegram 私聊交互」：开启交互（会用 `getMe` 校验 Token，并记录新的接收起点：此前积压的消息不会触发管理动作）。点击「生成绑定码」，5 分钟内在 Telegram 私聊 Bot 发送页面显示的 `/bind <绑定码>`。绑定码只显示一次、只能使用一次。
6. 绑定不会自动打开邮箱写入、扩大自动化策略或启用任何任务。

## 在 Telegram 中使用

- 直接描述需求，例如“把 test@example.com 超过 30 天的邮件移到回收站，保留未读和星标，每周五 20 点执行”。缺少账号、发件人地址、文件夹或时间时，Agent 会追问而不是猜测。
- 新规则流程：保存**禁用**规则草稿 → 只读试运行（在内存中按“若启用”评估，不临时启用规则，不写原邮箱）→ 发送确认卡片。卡片列出准确条件、原邮箱目的文件夹、保护、有效每批数量、时区和未来 5 次日程、本次扫描统计（不是全邮箱总量）和受影响的现有任务。点击「确认执行」后按“启用规则 → 创建暂停观察任务 → 切换执行模式 → 启用任务”逐步生效，并回报每一步的实际 ID 与版本。
- 补充条件或发送任何新的普通消息会立即使旧卡片失效（命令不会）。卡片 10 分钟过期；规则、任务、策略或文件夹映射被他人修改后，旧卡片也会失效。
- “立即执行某任务”：任务已启用且版本未变时直接入队一次；暂停中的任务会先发确认卡片。运行开始与结束由服务端按真实运行记录推送：已核验完成、失败、待核验（结果未知）、保护跳过、未执行分别列出，未知结果不算成功。
- 命令（不依赖模型）：`/status` 服务、模型、授权与最近运行；`/new` 开始新会话（旧上下文不再提供给模型，待确认卡片失效）；`/stop` 停止当前与排队的 Agent 处理和未开始的方案步骤（不是回滚）；`/cancel <run_id>` 停止某次运行的剩余项（可用通知中的 8 位前缀）；`/help`。
- 只接受已绑定私聊中真实发送者的原创文本与按钮。群聊、频道、转发、编辑消息、匿名发送者、附件、语音、inline 模式均被忽略；未绑定用户只会收到绑定提示，消息不进入模型。

## 安全边界

- 身份由服务端根据有效绑定解析为 `agent:<grant_id>`；模型只能提交业务参数，提交 owner、授权或“已确认”等字段会被参数校验拒绝。模型不能调用确认操作。
- 每次工具调用、确认、入队和写入都重查授权：授权撤销、过期、收窄或管理员凭据变化后，下一次写入被拒绝，待确认方案失效；授权范围变化时旧对话上下文不再发送给模型。已经在途的远端写入继续走原有核验路径，不重放 MOVE。
- 工具白名单（18 个）：`list_accounts`、`list_folders`、`get_policy`、`list_rules`、`get_rule`、`list_tasks`、`get_task`、`list_task_runs`、`get_task_run`、`preview_task`、`save_rule_draft`、`save_task_draft`、`propose_workflow`、`propose_rule_change`、`propose_task_change`、`run_task`、`pause_task`、`cancel_task_run`。不提供策略修改、管理员/API/Bot 凭据、原始 IMAP、`mail_execute`、`mail_ai_*`、发信或永久删除。
- 工具参数严格校验：未知工具、未知字段、尾随 JSON、类型错误、过深嵌套、超过 32 KiB 的参数一律拒绝；被截断或无效的模型响应不会触发任何写入。工具结果超过 32 KiB 时报错而不是静默截断。
- 邮件主题、发件人、规则名称作为不可信数据，不会被当作指令。首版不读取正文。
- 模型地址仅允许 HTTPS 公网：连接时校验实际 IP（DNS 重绑定、IPv6、IPv4 映射地址），拒绝回环、私网、链路本地/元数据、CGNAT、文档与 NAT64/6to4 前缀；不跟随重定向；不读取 `HTTP(S)_PROXY`。如需本地模型或代理，需要后续版本显式实现白名单。
- API Key、Bot Token、邮箱凭据不进入模型上下文、Telegram 回复、普通日志或 API 响应。对话、工具结果、方案与回复载荷用主密钥加密（AAD 绑定记录 ID）。
- 单次工作上限：单次模型请求 10–120 秒（默认 60），单轮 120 秒、最多 8 次模型请求与 20 次工具调用，上下文最多 20 轮、约 64 KiB，全局同时一个模型轮次、最多 100 个排队轮次。这些是单次工作上限，不是每日邮件额度；系统不设置每日处理额度。

## 可靠性语义

- Telegram 更新：每批更新与 `next_offset` 同一事务提交；写库失败不推进 offset，下一次拉取得到同一批，`UNIQUE(bot_id,update_id)` 去重。update_id 有间隔时按实际最大值推进。webhook 已设置或另一个消费者（409）时报告冲突并退避，不删除 webhook、不丢弃待处理更新。同一数据库的消费者使用租约与代际围栏；不同数据库同时使用同一 Token 只能检测到冲突。Telegram 服务端最多保留 24 小时更新。
- 业务幂等：规则/任务写入使用服务端 mutation ID，业务写入、审计与幂等结果同一事务提交；同一 Telegram 更新重复投递不会产生第二个轮次；同一轮次重新生成的相同调用复用原操作；双击确认按钮由 CAS 消费一次；运行键由服务端操作 ID 生成。
- 恢复：重启时正在处理的对话标记为中断并告知已完成的写入，不自动重跑；已确认的方案按幂等步骤继续，`/stop` 的停止代际优先。多步骤方案部分完成时如实报告已完成资源，不自动回滚。
- 消息投递：发送超时可能已送达，标记“投递结果未知”最多再试一次，可能出现少量重复提示，但从不因此重做业务写入。
- 禁用 Agent 或关闭交互会取消模型请求与未开始的 Agent 工作并使方案失效；已启用的固定任务继续按原授权运行。

## 管理 API（均需管理员会话、同源与 CSRF）

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/agent/provider` | 脱敏配置、最终请求地址、`key_configured`、当前配置的检测结果 |
| `PUT /api/agent/provider` | `{version, enabled, base_url, model, timeout_seconds, max_tokens_param, api_key, clear_key}`；乐观锁；空 `api_key` 表示保留 |
| `POST /api/agent/provider/check` | `{kind: "chat" \| "tool_loop"}`；只执行本地无副作用的 `echo` 工具 |
| `GET /api/agent/grant` | 当前授权与可选动作 |
| `PUT /api/agent/grant` | `{version, enabled, days, accounts, folders, actions}` |
| `POST /api/agent/grant/revoke` | 撤销授权、解除绑定、取消排队工作、使方案失效 |
| `GET /api/telegram/interaction` | 交互开关、Bot、接收状态、绑定 |
| `PUT /api/telegram/interaction` | `{version, enabled}` |
| `POST /api/telegram/bind-code` | 生成一次性绑定码（不能通过 GET 重读） |
| `POST /api/telegram/unbind` | 解除绑定并取消未开始的工作 |
| `GET /api/agent/conversations` | 轮次、方案与关联运行摘要（不含原文） |
| `GET /api/agent/turns/{id}` | 工具名称、状态、结果码、操作 ID 与业务对象 |
| `POST /api/agent/turns/{id}/stop` | 与 `/stop` 语义相同 |

错误响应为 `{"code": "...", "error": "中文说明"}`，固定码包括 `tool_call_unsupported`、`provider_auth_failed`、`provider_rate_limited`、`provider_destination_blocked`、`grant_required`、`state_conflict`、`permission_denied`、`invalid_input`；Telegram 侧为 `telegram_token_invalid`、`telegram_consumer_conflict` 等。

## 数据保留

对话、工具结果、方案与回复载荷 7 天后清理；幂等记录、最小审计与 inbox 墓碑 60 天。待确认/执行中的方案、排队或处理中的轮次、未投递回复、未处理 inbox 与未结束的运行关联不按年龄删除。`/new` 只让旧上下文不再使用，不删除任务或审计；删除记录不等于磁盘安全擦除。

## 代码位置

- `internal/modelprovider`：Provider 接口、Chat Completions、目的地址防护、重试、检测。
- `internal/mailtools`：工具注册、严格参数解码、草稿/方案/运行工具、方案步骤执行。
- `internal/agent`：Provider 设置、轮次队列与模型循环、方案确认与执行、停止/恢复、运行结果推送。
- `internal/telegram`：Bot API 客户端、绑定、长轮询与 inbox、命令与回调、回复 outbox。
- `internal/interaction`：绑定、会话代际与回复 outbox 的共享持久化契约。
- `internal/mail/agent_access.go`：Agent 授权与身份校验、跨 owner 运行管理、mutation 幂等、草稿工作流预览与影响分析。
- `internal/store/agent.go`：schema 10 表与保留期清理。
- `internal/admin/agent.go`、`internal/admin/web/agent.js`：管理 API 与页面。

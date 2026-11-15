# 0.8.1 验收材料：0.8.0 审计五项修复（S2-01～S2-05）

本版只修复 Codex《Mail Auto 0.8.0 独立验收报告》（2026-11-15）列出的五项问题，没有新增功能，也没有扩大改造范围。

- 未新增框架、服务、依赖、数据库迁移（仍为 schema 13）、通用状态管理或浏览器持久化草稿。
- 现有任务、每批数量、未读/星标保护、历史范围、通知目的地和模型连接均未改动，也不设置每日处理额度。

文中证据标注：

- **[模拟]**：浏览器加载真实页面脚本，接口替换为内存假数据。
- **[本地真实后端]**：Go 测试使用真实 Service 与 SQLite，或使用本机临时实例。
- **[生产只读]**：只读查询 Oracle1。
- **[用户确认]**：由用户告知，执行者未独立复核。

## 1. 提交、版本与范围

- 版本 0.8.1。发布提交与标签见第 4 节。
- 修改的产品文件：
  - 前端：`internal/admin/web/app.js`、`automation.js`、`rules.js`、`tasks.js`、`organizer.js`、`index.html`。
  - 后端：`internal/mail/task_readiness.go`、`internal/mail/automation_policy.go`。
- 新增回归：
  - 后端测试 `internal/mail/task_readiness_test.go` 中的 `TestReadinessChecksEveryGrantSourceLikeRun`。
  - 界面检查 `scripts/ui-check/ownership-ui-check.js`，共 43 项。
- 文档：`CHANGELOG.md`、`README.md`（版本号）、本文件。

## 2. 五项问题

### S2-01 / P1：跨邮箱迟到响应覆盖与错账号提交

**根因**

- 摘要、规则、任务、文件夹设置四个载入函数各自读取隐藏下拉框，响应返回后直接写入共享变量（`rules` / `policy` / `capabilities` / `tasks`）和 DOM，没有检查响应是否仍属于当前页面。
- 授权保存用的是“当前选择器的账号路径 + 旧的 `policy` 内容”，后端又以 URL 中的账号覆盖 body 中的账号，所以旧账号的内容会被写到新账号。

**修改**

- `app.js` 新增 `autoScope`，记录当前邮箱、载入代际 `gen` 和切换代际 `epoch`，并提供四个小函数：
  - `autoTicket` / `autoCurrent`：载入用。只有 `gen` 与邮箱都还是当前值时，响应才会被应用。
  - `autoSameAccount`：保存用。只有仍在同一次切换内时，才允许改动页面。
  - `autoFormAccount`：表单必须已经按当前邮箱载入，才能提交。
- `automation.js` 的 `show()` 是唯一的载入入口：
  - 每次载入（刷新或切换）都递增 `gen`。
  - 切换时还会递增 `epoch`，清除授权表单与文件夹表单的 `data-account`，并关闭规则 / 任务编辑器；关闭前已确认放弃修改。
- `rules.js`、`tasks.js`、`organizer.js` 与摘要的载入函数，在改动共享状态或 DOM 前都会检查票据。
- 授权与文件夹设置的保存：
  - 使用表单所属的邮箱发送请求，切换尚未完成时拒绝提交。
  - 响应返回时，如果页面已经切换到其他邮箱，只提示“已保存到 A”，不改动 B 的页面。
- 规则保存、启停后的列表重载，以及文件夹探测、创建的回调，都做同样的检查。
- 这些检查都不依赖版本号。

**修复前后（Codex 原探针 `browser-probes.js`，同一本地实例，[模拟]）**

- 修复前（v0.8.0 二进制）：可见选择器为 a2，摘要、规则、任务都变回 A，写入开关为开。随后的保存是 `PUT /api/automation/policy/a2`，body 中 `account_id=a1`。与审计报告一致。
- 修复后：原探针在第一个“编辑器应关闭”的等待处超时。这是因为窗口按预期保持打开（见 S2-02），说明该探针断言的是缺陷行为，不能当作通过依据。修复后的正确行为由下面的新检查断言。

**回归（`ownership-ui-check.js`，[模拟]，两邮箱的授权与文件夹设置版本号相同）**

- 刷新 A 时延迟 A 的响应，切到 B 后再放行：选择器、摘要、规则、任务、文件夹仍全部是 B，每批 8、写入关闭。
- 随后保存授权：`PUT /policy/a2`，内容是 B 的。保存文件夹设置：`PUT /folder-settings/a2`。
- A→B→A，B 的响应迟到：页面仍完整是 A。
- 切换加载期间提交授权或文件夹表单：一个请求也不发送，并提示等待加载。加载完成后可以正常保存到 B。
- 保存 A 的授权期间切到 B：
  - 请求仍是 A 的 URL 和 A 的内容。
  - 响应不改动 B 的页面，并提示已保存到“A邮箱”。

### S2-02 / P2：三个编辑器丢失保存期间的新修改

**根因**

邮箱、规则、任务编辑器在保存成功后无条件执行 `reset` / `close`。它们没有使用已有的草稿代际，也无法分辨对话框是否已经关闭后又重新打开。

**修改**

- `app.js`：
  - 草稿记录新增 `session`，对话框每次打开（`showModal`）时同步加一，并从干净状态开始。
  - 对话框的 close 事件是排队异步触发的。如果事件到达时对话框已经重新打开，就视为过期事件，不清除新打开窗口的草稿。
  - `editorTicket(form)` 在提交时记下草稿代际和 `session`。
  - `editorState(ticket)` 在响应返回时判断状态：
    - `same`：没有后续编辑。
    - `edited`：保存期间有新编辑。
    - `closed`：已关闭，可能又已重新打开。对话框已经关闭也算这种情况，因为 close 事件是异步排队的。
- 三个编辑器统一按状态处理：
  - `same`：清理草稿并关闭窗口，与原来一致。
  - `edited`：保留窗口和新的修改，在窗口内及通知中提示“保存期间的新修改尚未保存”。同时更新已保存记录：
    - 邮箱：把 `state.editingAccount` 改为新 ID。
    - 规则 / 任务：更新正在编辑对象的 `id` 与 `version`，标题和按钮改为“编辑”。
    - 这样再次保存是 `PUT` 同一记录，不会重复新建，也不会因版本过期而冲突。
  - `closed`：只刷新列表并提示已保存，不关闭、不清理、不改动新打开的编辑。
- 各编辑器只修改提交时捕获的对象，不碰之后打开的新对象。

**修复前后**

- 修复前（原探针，v0.8.0）：三个编辑器都关闭，`dirty=false`，邮箱字段被清空，没有任何提示。
- 修复后（新检查，[模拟]），对邮箱（新建）、规则（已有）、规则（新建）、任务（已有）四种情况各验证四点：
  - 保存期间继续编辑后，窗口保持打开，草稿保留，并提示仍有修改。
  - 首次请求提交的是提交时的内容。
  - 再次保存更新同一记录：
    - `POST /api/accounts` → `PUT /api/accounts/a9`
    - `PUT /api/rules/r-a1`，版本 2
    - `POST /api/rules` → `PUT /api/rules/r-new`，版本 1
    - `PUT /api/tasks/t-a1`，版本 2
  - 再次保存时没有新的编辑，窗口正常关闭，草稿被清理。
- 邮箱、规则、任务三个编辑器：保存期间关闭旧窗口并重新打开、输入新内容后，旧请求的回调不关闭、不清理、不改动新的编辑。
- 关闭后立即重开（同一个任务内）：迟到的 close 事件不清除新的草稿。
  - 自测中发现的问题：第一版按 close 事件计数打开次数，多次重复运行时，“新规则”与“保存期间点按钮”两项会间歇失败（9 次宽度运行中失败 4 次）。原因是上一次打开的 close 事件晚于重新打开才到达。
  - 改为在 `showModal` 时计数后，连续 5 轮 × 3 个宽度全部通过。
  - 同一检查在去掉这处修改的构建上，三个宽度都失败。

### S2-03 / P2：规则按钮操作没有标记草稿

**根因**

添加、删除条件或子组只改动内存中的 `match` 对象并重绘，不触发 `input` / `change` 事件，草稿保护监听不到。

**修改**

- `app.js` 新增 `touchDraft(form)`，与输入事件共用同一个计数入口。
- 下面这些实际修改都会调用它：
  - `rules.js` 的“＋ 条件”“＋ 子条件组”“移除”“移除此子组”。
  - “应用模板”。
- 打开编辑器和重绘本身不会标记修改。

**修复前后**

- 修复前（原探针）：只点“移除条件”后 `dirty=false`，关闭时不询问。
- 修复后（新检查，[模拟]）：
  - 移除条件即算未保存修改。
  - 关闭按钮会询问，取消后窗口与修改都保留。
  - Esc 会询问，取消后不关闭。
  - 离开页面时 `beforeunload` 会提示。
  - 添加条件、添加或移除子组都算修改。
  - 仅打开编辑器不算修改。
  - 保存期间点击按钮修改后，窗口保持打开，仍为未保存。

### S2-04 / P2：就绪检查与实际运行来源校验不一致

**根因**

- 实际运行时，`authorizedAutomation` 会检查整理授权中的全部源目录：每个都必须可读，并且在邮箱同步范围内。
- 就绪检查只遍历任务自己的源目录。

**修改**

- 在 `task_readiness.go` 中提取 `grantSourceIssues(a, account, policy)`：
  - 目录不可读：报 `source_not_readable`。
  - 目录不在同步范围：报 `source_not_synced`，并带上目录名。
- `authorizedAutomation` 改为调用这个函数，判定和顺序不变，权限也没有放宽。
- 就绪检查调用同一个函数。任务源目录如果不在授权中且也不同步，仍单独报出，并去重。
- 任务卡的说明改为指出授权中不再同步的目录，并提供“编辑邮箱”（恢复同步）和“打开整理授权”（移除该目录）两个入口。

**修复前后（Codex 原 Go 探针 `readiness_probe_test.go`，通过真实 `Service.Update` 收窄同步范围，[本地真实后端]）**

- 修复前：`readiness.ready=true issues=[]; actual RunTask=permission_denied`。
- 修复后：`readiness.ready=false issues=[source_not_synced]; actual RunTask=permission_denied`。原探针断言的是缺陷行为，因此现在失败，符合预期。

**回归：`TestReadinessChecksEveryGrantSourceLikeRun`（[本地真实后端]）**

- 授权源目录为 `INBOX, Other`，任务只用 `INBOX`。通过真实 `Service.Update` 把同步范围收窄为 `INBOX` 后：
  - 就绪检查报 `source_not_synced`，目标为 `Other`，入口为 `account`。
  - 检查前后数据库计数不变。
  - `RunTask` 被拒，结果为 `ErrDenied`。
  - 共用的准入函数给出相同的拒绝原因。
- 恢复全量同步后，就绪检查与运行都不再阻塞。
- 页面文案与两个入口由 `ownership-ui-check.js` 断言（[模拟]）。

### S2-05 / P2：旧运行链接导致页面账号不一致

**根因**

`openTaskRun` 只修改隐藏的任务下拉框并重载任务区；选择器、摘要、规则和文件夹仍属于原来的邮箱。

**修改**

- `automation.js` 新增 `selectAutomationAccount(id)`，与邮箱选择器走同一个 `show()` 流程。
  - 放弃修改前会先确认，确认范围包括当前页面、自动整理页和两个编辑器。
  - 如果从其他页面进入，会先进入自动整理页，并直接载入目标邮箱。
- `openTaskRun` 改为调用 `selectAutomationAccount`，整页载入完成后再定位任务与运行记录。
- 用户取消时，保持原邮箱和草稿，并提示没有打开该运行记录。

**修复前后**

- 修复前（原探针）：选择器、规则账号、文件夹账号都是 a1，摘要为 A；只有任务账号是 a2。
- 修复后（新检查，[模拟]）：
  - 有未保存授权时打开 B 的链接并取消：询问一次，页面仍完整是 A，授权表单中的 17 和未保存状态都保留。
  - 确认后：选择器、摘要、规则、任务、文件夹设置以及两个表单的 `data-account` 都是 B，并定位到“任务-a2”的运行记录。
  - 从仪表盘打开 B 的链接：进入自动整理页，整页为 B。
  - 打开同一邮箱的链接：不询问，直接定位。

## 3. 测试

所有检查都在本仓库 worktree 中运行。该 worktree 不含 `.planning/`，相当于干净的源码副本，没有修改或删除任何用户文件。

审计报告中 `make check` 的格式阶段被阻断，原因是主仓库目录下两份既有、未跟踪的 `.planning/.../crash_flags_probe_test.go` 诊断文件未格式化。这属于环境问题，不是产品问题；那两个文件仍保持原样，未作改动。

| 命令 | 退出状态 / 结果 |
| --- | --- |
| `make check`（`go test -race ./...`、`go vet ./...`、`gofmt -l .`、`staticcheck@v0.8.2 ./...`） | 0。修复代码完成后跑一次；`showModal` 计数修改与版本号更新后，又在最终源码上完整重跑一次（见 3.1） |
| `go test -race -count=1 ./internal/mail ./internal/admin ./internal/agent ./internal/modelprovider ./internal/store`（不使用缓存） | 0：mail 208.7s、admin 95.9s、agent 41.7s、modelprovider 2.2s、store 26.1s。最终 JS 修改后，admin 包又单独无缓存重跑一次，92.9s，结果 ok |
| `go test -count=1 ./internal/mail -run 'TestReadiness\|TestTaskPolicyIssues'` | ok，包括新增的 `TestReadinessChecksEveryGrantSourceLikeRun` |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` | 0，本地临时实例启动后报告 `version=0.8.1` |
| `git diff --check`、`gofmt -l .`、`docker compose config -q`、页面与检查脚本 `node --check` | 全部通过 |
| `settings-ui-check.js` **[模拟]** | 1280 / 375 / 390 px 各 45 项通过，连续 3 轮 |
| `model-ui-check.js` **[模拟]** | 1280 / 375 / 390 px 各 63 项通过，连续 3 轮 |
| `ownership-ui-check.js` **[模拟]**（新增） | 1280 / 375 / 390 px 各 43 项通过，最终构建上连续 3 轮；此前另有 5 轮 42 项版本全部通过 |
| 新检查在 v0.8.0 上的表现 **[模拟]** | 前 12 项中 8 项失败（S2-01 全部页面归属项、邮箱编辑器迟到保存），随后在首个编辑器流程中断。说明这些检查能发现原缺陷 |
| Codex 原浏览器探针 **[模拟]** | v0.8.0 上四项缺陷全部复现；修复后探针在“等待编辑器关闭”处超时，因为编辑器按预期保持打开 |
| Codex 原 Go 探针 **[本地真实后端]** | v0.8.0：`ready=true issues=[]`，`RunTask=permission_denied`；修复后：`ready=false issues=[source_not_synced]`，`RunTask=permission_denied` |
| 页面错误 | 所有界面运行中均为 0 |

UI 检查的运行方式：

- 在本机临时实例上执行：`mail-auto -data-dir <tmp> -key-dir <tmp> -listen 127.0.0.1:18082 -automatic-sync=false`，使用临时测试账号。
- 执行 `node scripts/ui-check/run-checks.cjs <puppeteer> http://127.0.0.1:18082 <临时凭据 JSON> scripts/ui-check/settings-ui-check.js scripts/ui-check/model-ui-check.js scripts/ui-check/ownership-ui-check.js`。
- 每个宽度都会重新加载页面。
- 多次重复运行会触发本地登录频率限制（“登录尝试过多”），重启临时实例即可。这是登录保护按设计工作，与本版无关。

**未验证**

- 五项界面修复只在模拟接口与本机临时实例上验证，没有在生产页面上人工操作。
- S2-04 的真实后端证据来自 Go 测试（真实 Service 与 SQLite）。没有对真实 IMAP 邮箱收窄同步范围。
- 新任务编辑器保存前的实时就绪检查，仍按审计意见保留为轻量边界，本版未新增。

### 3.1 命令记录

- 2026-11-15，修复代码完成后：
  - `make check`：exit 0。
  - 五包无缓存 race：exit 0。
  - 构建、diff、compose、js 检查全部 ok。
- 加入 `showModal` 计数并更新版本号后：
  - admin 包无缓存 race：ok。
  - `node --check`、`git diff --check`、`gofmt`：ok。
  - 三项界面检查连续 3 轮全部通过。
  - 最终源码完整 `make check`：exit 0（race、vet、gofmt、staticcheck）。

## 4. 标签、镜像与 Oracle1

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `62f86aa0cf68b5a91f00709059edbbeca51004d3`，已推送到 `origin/main` |
| 标签 | `v0.8.1` → `62f86aa0cf68b5a91f00709059edbbeca51004d3`（附注标签） |
| 镜像 index（`0.8.1` 与 `latest` 相同） | `sha256:b6476dadaaa96fd3039713387bad229f9aff70ef3ca8018196cda3a35461334c` |
| linux/amd64 | `sha256:fb2cd9486c3dd31a5b5cdadeff8a9f2b83f98c4f09b51eb8e86264565f986015` |
| linux/arm64 | `sha256:aa487bdeddc6eac7c7d45b300fafa05ebd62dfd9865652b931a037e2f624c727`，config `sha256:39aa8b7fa13609fcb1957d224292b6d9ebbc7d7f855fd29c6b537fd227830836` |
| index 中另两项 | `unknown/unknown`，为 buildx 构建证明（attestation），与 0.8.0 相同 |
| CI | GitHub Actions `ci`：main 43671271997 与 tag `v0.8.1` 43671284929 均为 success，均对应 `62f86aa` |
| Oracle1 运行镜像 **[生产只读]** | ImageID `sha256:39aa8b7f…0836`（arm64），与 arm64 config 一致；RepoDigest 为上方 index |
| 版本 / schema | `/healthz` 0.8.1 ok；`/readyz` 0.8.1、schema 13、ready。线上 `automation.js` 与 `app.js` 已包含 `selectAutomationAccount`、`autoScope`、`showModal` 计数等新代码 |
| 健康 / 重启 | healthy，重启 0，2026-11-15T03:13:52Z（UTC）启动 |
| 部署前检查 **[生产只读]** | 03:13:26Z：进行中运行 0、写入操作 0、待确认方案 0、Agent 轮次均已完成；下一个计划运行为 04:00Z |
| 邮箱同步 **[生产只读]** | 部署后 03:14:21–03:14:41Z，14 个文件夹全部完成同步 |
| 日志 | 部署后 error / panic 0 条 |
| 其他服务 | 23 个容器的 ID、重启次数、运行状态都与部署前清单一致 |
| 线上就绪显示 **[生产只读]** | 在已登录页面中进入“自动整理”，4 个既有任务都显示“✔ 运行条件已满足”，与就绪接口一致 |
| 任务结果 | **[用户授权的手动运行]** 用户在对话中明确要求手动触发后，执行者在用户已登录的 Chrome 中点击 C-RpBGXr（163 收件箱自动分类）的“手动运行”。<br>**[生产只读]** 03:41:26Z 运行 `NTCkaR_E…`：状态 `noop`，扫描 500、领取 0、完成 0、失败 0、待核验 0，通知 `not_required`。审计 1760–1762 依次为 `mail.task.run`（manual）→ `mail.ai.claim` ok → `mail.task.result` noop。部署后新建写入操作 0 个。<br>说明任务在 0.8.1 上可以端到端运行；本轮没有符合条件的邮件，**没有发生写入**，因此本轮不是写入证据，更不是模型分类写入证据。04:00Z 自然调度的结果见第 6 节 |

## 5. 升级前后配置对比、备份与清理

**配置对比 [生产只读]**

- 方法：用 0.8.0 验收材料第 4 节的同一只读脚本，比较升级前快照 `backups/upgrade-0.8.1-20261115-031349/mail-auto.db` 与部署后的数据库（03:15Z）。
- 结果：**25 项全部相同**，`runtime_changed` 为空。
- 覆盖范围：
  - 4 个任务：每批数量、保护、历史起点、规则绑定、通知策略、启用状态与触发方式。
  - 整理授权：每批 20、保护未读与星标、历史范围、无每日额度。
  - 10 条规则、文件夹设置、账号（密钥只比摘要）。
  - 通知通道：Chat ID 与密钥摘要不变。
  - Telegram 交互与绑定、网络设置、Agent 授权。
  - 模型当前配置：连接 A，`auto/gpt-6-luna`，版本 5，以及两个模型连接。
  - schema 13。

**备份与回滚**

- 升级前备份 `backups/upgrade-0.8.1-20261115-031349`，权限 `go-rwx`，内容包括：
  - 一致性数据库快照：integrity ok，schema 13，计数为邮箱 1、邮件 1819、文件夹 14、规则 10、任务 4、运行 119、模型连接 2。
  - 主密钥。
  - compose 与环境文件。
  - 清单（含其他 23 个服务的基线）。
- 回滚：0.8.1 无迁移，可直接改回 `allen0039/mail-auto:0.8.0`（已保留）。如需回到快照，先停止容器，再恢复数据库与配套主密钥。本次未做恢复演练。

**清理（仅限确认属于本项目且已过期的资源）**

- 删除：
  - 镜像 `allen0039/mail-auto:0.7.2`：确认没有容器使用；回滚镜像改为 0.8.0。
  - 备份 `backups/upgrade-0.8.0-20261114-161348`：已由 0.8.1 升级前备份取代。删除前，新备份已通过 quick_check ok 与 schema 13 检查。
- 保留：
  - 镜像 0.8.1 / latest（`39aa8b7f`）与 0.8.0（`72e3ec61`）。
  - 0.8.1 升级前备份、全部发布记录（新增 `releases/0.8.1.json`）、部署配置、邮件数据与密钥。
- 本项目没有悬空镜像，其他服务未作处理。
- 本地：
  - 临时测试实例已停止。
  - 临时数据库、主密钥、测试凭据、用于复现的 v0.8.0 源码副本与二进制、调试脚本均已删除。
  - 检查结果 JSON 保留在会话临时目录，不入库。

## 6. 未验证与说明

- 五项界面修复的证据是 **[模拟]**（真实页面脚本 + 假接口）。S2-04 的证据是 **[本地真实后端]**（Go 测试）。执行者没有在生产页面上人工操作，也没有修改生产配置。
- 生产操作：只有一次 C-RpBGXr 手动运行，由用户在对话中明确要求（见第 4 节“任务结果”），结果为 noop，没有写入。此外没有手动执行其他生产任务，也没有修改日程、授权或通知目的地。（0.8.2 勘误：此前这里写成“没有手动执行生产任务”，与第 4 节矛盾。）
- 模型分类写入：此前为 **[用户确认]**，执行者未独立复核。本次部署后的计划任务是固定规则任务；即使发生写入，也不能作为模型分类写入的证据。
- 既有的 IMAP 间歇断开 / 超时问题不在本版范围内。
- Codex 原浏览器探针断言的是缺陷行为，修复后在等待处超时属于预期，不能把它记为“通过”。修复后的正确行为以 `ownership-ui-check.js` 与 `TestReadinessChecksEveryGrantSourceLikeRun` 为准。
- 04:00Z 自然调度补记 **[生产只读]**（04:00:47Z 读取）：
  - C-RpBGXr 内置定时运行 `3Y9WqfzB…`（04:00:02Z）：状态 `noop`，扫描 259、领取 0、完成 0、失败 0、待核验 0，通知 `not_required`。审计 1763–1765 依次为 `mail.task.run`（internal）→ `mail.ai.claim` ok → `mail.task.result` noop。
  - 部署后写入操作仍为 0 个。
  - 14 个文件夹在 03:59:18–03:59:45Z 再次完成同步；healthy，重启 0，error / panic 0 条。
  - CI：文档提交 `da5a514` 在 main 上为 success（43673563031）。
  - **结论：** 截至本次记录，0.8.1 上的计划运行与手动运行都正常结束，但都没有可处理的邮件，**没有新的真实写入证据**。0.8.0 时的固定规则写入（17:00Z、00:10Z）由审计报告独立复核；之后的写入要等新邮件到达时的自然调度。
- 本文件不声明验收通过，交由 Codex 独立复验。

# 0.8.8 验收记录：DP-01 补修

范围：只修复 0.8.7 独立审计 DP-01（P2，邮箱窗口草稿保护）。

- 不重做已通过的 LE-01、LE-02，不新增功能、接口、依赖或持久化草稿。
- 只改 `internal/admin/web/app.js` 中邮箱窗口的两处处理，另有回归与版本文件。
- 数据库仍为 schema 13，无迁移。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[模拟]** 隔离临时实例 + 一次性凭据 + 假数据接口；**[生产只读]** Oracle1 只读查询；**[真实写入]** 原邮箱实际写入（本版没有）。

## 1. 根因与修复

**根因**

- `<dialog>` 的 `close` 事件是排队派发的。
- 页面级 `close` 处理（`app.js:139`）已经判断“事件到达时窗口已重新打开则视为过期”。
- 邮箱窗口专属的 `close` 处理没有这个判断，无条件执行 `form.reset()` 和 `state.editingAccount = null`。
- 结果：关闭后在同一事件循环内重开（已有邮箱或新建），迟到的旧事件会清空新窗口字段，并把编辑身份清为 null。草稿标记仍为 dirty，下一次保存会把已有邮箱当作新建。

**修复 1（`app.js:939`）**

- 邮箱窗口的 `close` 处理首行加 `if (e.currentTarget.open) return;`，与页面级规则相同。
- 正常关闭时窗口已关闭，仍清空表单（含授权码）并清除编辑 ID。
- 没有固定等待，不禁用继续编辑，不重置新草稿。

**修复 2（`app.js:1158`，同一边界的必要补充）**

- 验收要求第 4 项需要覆盖“旧保存失败在立即重开后返回”，新回归发现 `catch` 无条件把错误写入当前窗口的错误栏，会污染新窗口的提示。
- 成功回调早已按 `editorState(ticket)` 判断编辑会话；现在失败回调使用同一判断：
  - 编辑会话已关闭（含重开）时，改为页面提示“邮箱配置未保存：…”；
  - 仍是同一会话时，行为不变（错误写入窗口，草稿保留）。

## 2. 修复前后复现

| 检查 | 修复前（0.8.7 代码） | 修复后（0.8.8） |
| --- | --- | --- |
| 审计探针 `account-close-check.js` **[模拟]** | exit 1；三个宽度均 3/6，失败项与审计报告相同（已有→已有：字段被清空、编辑身份变为 null；已有→新建：字段被清空） | exit 0；三个宽度均 6/6 |
| 新版 `ownership-ui-check.js` **[模拟]** | exit 1；三个宽度均 81/87，失败的 6 项正是新增的 DP-01 用例 | exit 0；三个宽度均 87/87 |

新增 DP-01 用例（`ownership-ui-check.js` “DP-01” 段，共 10 项，77 → 87，三个宽度各跑一次）：

1. 已有邮箱 a1 关闭后，同一任务内重开另一已有邮箱 a2（2 项）：
   - 120 ms 后字段、未保存标记保留；
   - 编辑身份为 a2，标题为“编辑邮箱配置”。
2. 已有邮箱关闭后立即打开新建邮箱（2 项）：
   - 较新草稿保留；
   - 编辑身份为新建（null），标题为“添加邮箱”。
3. 正常关闭（1 项）：授权码、名称清空，编辑身份清除。
4. 正常保存（1 项）：POST 一次，窗口关闭，授权码清空，草稿干净。
5. 旧保存在立即重开 a2 后返回，成功与失败各 2 项（共 4 项）：
   - 新窗口字段、未保存标记、编辑身份 a2、标题、错误栏均不变。
   - 请求仍是 `PUT /api/accounts/a1`，带旧窗口提交的内容；结果以页面提示告知。

在 0.8.7 上，这 10 项中有 6 项失败：

- 用例 1 的 2 项；
- 用例 2 的字段项；
- 用例 5 中“成功”的窗口项：旧关闭事件清空了新窗口；
- 用例 5 中“失败”的 2 项：错误写入了新窗口。

其余 4 项通过：新建身份本来就是 null；正常关闭、正常保存行为不变；成功时的提示本来就有。

既有用例保留未改，包括原有“关闭后等待再重开”的旧请求用例（S2-02 邮箱／规则／任务）。本次没有删除、放宽或跳过任何断言。

复现步骤：

1. `npm i puppeteer-core`。
2. `PUPPETEER=<路径>/node_modules/puppeteer-core scripts/ui-check/run.sh .planning/audit-0.8.7-20261115/account-close-check.js`。
3. 再运行 `scripts/ui-check/run.sh scripts/ui-check/ownership-ui-check.js`。
4. 修复前的结果可在 v0.8.7 检出副本中用同一检查文件得到。

## 3. 测试

| 命令 | 结果 |
| --- | --- |
| `make check`（race 测试、vet、gofmt、staticcheck@v0.8.2） | exit 0。本 worktree 不含 `.planning/` 等未跟踪文件，相当于干净提交副本 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.8` |
| `node --check`（全部页面与检查脚本）、`bash -n run.sh`、`git diff --check`、`docker compose config -q`、`gofmt -l` | 全部通过 |
| `run.sh`（默认四套）**[模拟]** ×2，`CHROME_PATH=<本机 Chrome> run.sh` ×1 | 均 exit 0。每个宽度（1280 / 375 / 390）：settings 45/45、model 63/63、ownership 87/87、task-editor 68/68，页面错误 0 |
| `PUPPETEER=… node --test scripts/ui-check/gate-test.cjs` **[模拟]** | 27/27，fail 0，skipped 0（LG-01 两个反例、普通失败、页面异常、执行异常、零检查均非零退出） |
| 审计探针 `audit-0.8.6-20261115/task-edge-check.js` **[模拟]** | exit 0；三个宽度均 12/12。覆盖 LE-01、LE-02 与任务草稿保护：已有配置原样提交、保存中编辑、版本更新、未知错误脱敏、A→B→A |
| 审计探针 `audit-0.8.7-20261115/account-close-check.js` **[模拟]** | exit 0；三个宽度均 6/6 |
| 运行后资源 | `$TMPDIR` 下无 `mail-auto-ui-check.*` 或 `gate-fixture-*`，无 headless Chrome，无临时实例 |

LE-01、LE-02 的回归包含在 `task-editor-ui-check.js` 68 项中，本版未改动 `tasks.js`。

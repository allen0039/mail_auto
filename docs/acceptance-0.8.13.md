# 0.8.13 验收记录：在原任务中明确确认采用当前规则版本（方案第三项 C）

范围：只实施轻量优化方案第三项 C。

- 沿用现有任务编辑器、规则读取 API（`GET /api/rules`）、任务保存 API（`PUT /api/tasks/{id}`）和后端校验。
- 后端代码、数据库（schema 13，无迁移）、任务运行、同步、授权、通知都不变；没有新增依赖、后台服务或工作流；未调用 REA。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[模拟]** 隔离临时实例、一次性凭据和页面内假接口；**[单元]** Go 测试；**[CI]** GitHub Actions；**[生产只读]** Oracle1 只读查询。

## 1. 变更范围与关键文件

| 文件 | 改动 |
| --- | --- |
| `internal/admin/web/tasks.js` | `renderRuleVersions` 显示版本；`adoptBlocker` 判断能否采用；“确认采用当前规则版本”点击处理；`boundAtOpen` 记录打开（或保存）时的绑定，只用于显示；保存遇到 `rules_changed` 时的说明与规则刷新 |
| `internal/admin/web/index.html` | 规则选择下方新增 `#task-rule-versions` 容器；版本号 v0.8.13 |
| `internal/admin/web/style.css` | `.task-rule-versions` 少量样式 |
| `scripts/ui-check/task-editor-ui-check.js` | 新增第 9 节（item C），68 → 99 项；假接口增加按用例改规则和按账号延迟规则响应的能力 |
| `internal/mail/tasks_test.go` | 新增 `TestTaskSaveAdoptsCurrentRuleVersionOnlyWhenSent`（锁定后端保存契约，后端代码未改） |
| `docs/automation-guide.md`、`CHANGELOG.md`、版本文件 | 说明与版本 |

## 2. 各项要求与实现对应

| 要求 | 实现 |
| --- | --- |
| 1. 显示绑定版本、当前版本、动作与目标 | 绑定指定规则时，规则选择下方逐条列出，如“账单归档：绑定 v2，当前 v3 · 移到指定目录 → …”；已是当前版本时显示“（当前版本）”，新选中的规则显示“新选择，保存时绑定当前 vN”；已停用、不适用、未选中会附注 |
| 2. 条件满足时提供“确认采用当前规则版本” | 有过期版本，且 `adoptBlocker` 返回空时才显示按钮。条件与服务器保存检查（`resolveTaskRulesFrom`）相同：原绑定的规则都还在选择中（未删除、仍适用于源目录）；所选规则存在且启用；适用于所选源目录；源目录内优先级更高或相同的启用规则都已绑定。另外要求规则列表属于当前编辑任务的邮箱，并且是当前切换后加载的 |
| 3. 只有点击才更新草稿并标为未保存 | 点击时再检查一次条件，然后只把所选规则的当前版本合并进 `editing.rule_versions`，调用 `touchDraft` 标为未保存；不发送请求，仍需“保存修改” |
| 4. 不点击不自动替换 | 打开、刷新列表、展开更多设置、修改其他字段、保存都不写 `rule_versions`。保存时沿用原逻辑 `editing.rule_versions[id] \|\| rule.version`，旧绑定照常提交，由服务器检查 |
| 5. 只改版本绑定 | 合并只涉及所选规则 ID 的版本；任务 ID、规则 ID 集合、源目录、每批、日程、保护、历史、通知、启用状态及其他未保存字段都不动 |
| 6. 不改全部规则、不新建、不自动保存、不运行 | 点击处理只改内存草稿 |
| 7. 删除、停用、不适用、缺少更高优先级规则时继续阻断 | 不提供按钮，显示原因，并提示调整规则选择或改为跟随全部启用规则；保存仍由服务器拒绝 |
| 8. 并发变化：提示冲突、保留草稿、不重试、不静默更新 | 保存遇到 `rules_changed` 时，说明“规则列表已刷新：可…确认采用当前规则版本后再保存”，并刷新规则列表（`loadAccount`，不改编辑器草稿）；编辑器显示新的当前版本，需要再次点击确认 |
| 9. 迟到响应遵守账号与编辑会话保护 | 规则列表仍由 `loadAccount` 加载，带 `autoCurrent` 检查；编辑器打开时只重画版本显示，不写草稿；版本显示和确认都要求 `loaded.account === editing.account_id`；确认是同步操作，只作用于当前打开的编辑器 |

## 3. 测试

| 命令 | 结果 |
| --- | --- |
| `make check`（race 测试、vet、gofmt、staticcheck） | exit 0，含新增 Go 测试 |
| `go test ./internal/mail -run 'TestTaskSaveAdoptsCurrentRuleVersionOnlyWhenSent\|TestTaskExplicitRuleBinding\|TestTaskBindingCannotSkip'` **[单元]** | ok |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.13` |
| `node --check`（页面与检查脚本）、`bash -n run.sh`、`docker compose config -q`、`git diff --check` | 全部通过 |
| `python3 -m unittest discover -s scripts/release-verify` | Ran 45，OK |
| `scripts/ui-check/run.sh`（Chrome for Testing 156.0.8078.47）**[模拟]** | exit 0；每个宽度（1280／375／390）settings 45、model 63、ownership 87、task-editor **99**，页面错误 0 |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` **[模拟]** | tests 27，pass 27，fail 0，skipped 0 |
| 审计探针 task-edge-check（LE-01／LE-02）、account-close-check（DP-01）**[模拟]** | exit 0；三个宽度分别 12/12、6/6 |
| 新检查对 0.8.12 的 `tasks.js` **[模拟]** | exit 1：“版本显示”“提供确认”两项失败，随后点击不存在的按钮导致脚本异常。说明新检查确实依赖本功能 |
| 运行后资源 | 无 `mail-auto-ui-check.*`、`gate-fixture-*` 或实例进程 |

### 新增 UI 回归（task-editor 第 9 节，每个宽度 31 项）**[模拟]**

夹具中的 t-custom 绑定账单归档 v2（当前 v3）和通知归档 v5（当前）。

| 场景 | 断言 |
| --- | --- |
| 显示 | 列出“账单归档：绑定 v2，当前 v3”“通知归档：绑定 v5（当前版本）”和“移到指定目录 →”；提供确认按钮（`type=button`）；打开编辑器不算修改 |
| 未确认不替换 | 刷新列表、展开更多设置并修改每批数量后保存：`rule_versions`、`rule_ids` 与原任务相同，只有 max_batch 变化。原有用例“折叠后原样保存（含规则版本）”也保持通过 |
| 确认只改草稿 | 修改名称和通知后点击确认：没有发送请求；标为未保存；显示“已确认采用当前 v3（保存后生效）”，按钮消失；名称、通知、每批、Cron、时区不变；关闭时询问并保留 |
| 保存内容 | `PUT /api/tasks/t-custom`，id 与 version 6 不变；与原任务相比只有 `notify` 和 `rule_versions` 变化（名称为用户修改）；`rule_versions` 为 {r1:3, r2:5}，`rule_ids` 不变；只发一个请求，没有 POST 或运行 |
| 取消与重开 | 放弃后重开恢复为“绑定 v2，当前 v3”，需要重新确认；改为全部启用规则时不显示版本绑定 |
| 阻断（4 种） | 规则已删除、已停用、改到不相关目录、新增优先级更高的启用规则：都不提供确认，警告说明原因；保存仍提交旧版本 r1:2，编辑器保持打开 |
| 确认前规则再变 | 刷新后显示“绑定 v2，当前 v4”，草稿名称不变；确认后带 v4 |
| 确认后规则再变 | 保存得到 `rules_changed`：说明含“确认采用当前规则版本”和“跟随账号全部启用规则”；草稿与未保存标记保留；只发一次请求；刷新后显示“绑定 v4，当前 v5”并可再次确认；再次确认后保存带 v5 |
| 迟到的规则列表（同一编辑器） | 确认后让刷新的规则响应延迟到达（r1 v9）：只更新显示，保存仍带确认时的 v3 |
| A→B→A | A 的规则响应在切到 B、打开任务-B 后才到达：B 的页面和编辑器不变，版本区隐藏，没有未保存标记；切回 A 后按 A 的当前规则显示并可确认 |
| 保存期间确认 | 保存中点击确认：已保存的请求是旧 v2，编辑器保留确认后的草稿并提示仍未保存；下次保存 PUT 同一任务新版本 7，带 v3 |
| 确认后保存期间继续编辑 | 已保存 v3，较新的名称保留，显示“绑定 v3（当前版本）” |

### 后端契约 **[单元]**

`TestTaskSaveAdoptsCurrentRuleVersionOnlyWhenSent`：

- 规则修改后，按旧绑定保存同一任务 → `ErrConflict`；
- 同一任务只换成当前版本 → 保存成功，ID、规则数量、每批 7、模式、启用状态不变；
- 规则停用后 → `ErrInput`。

既有的 `TestTaskExplicitRuleBindingSnapshotsVersions`（旧绑定不能运行）和 `TestTaskBindingCannotSkipHigherPriorityRule` 照常通过。

### 真实 CI **[CI]**

| 运行 | 提交 | 结果 |
| --- | --- | --- |
| 分支 [43737490625](https://github.com/allen0039/mail_auto/actions/runs/43737490625) | `887d7f1` | check、ui 成功；日志中 task-editor 三个宽度各 99 项，pageErrors 为空；门禁 pass 27、skipped 0；遗留 0；release-verify 夹具 Ran 45 |
| main [43738216956](https://github.com/allen0039/mail_auto/actions/runs/43738216956) | `887d7f1` | check=success，ui=success |
| tag v0.8.13 [43738219448](https://github.com/allen0039/mail_auto/actions/runs/43738219448) | `887d7f1` | check=success，ui=success |

## 4. 发布与镜像

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `887d7f1f38b1b9de4d01ff2279df54f115845332`，main 由 `0b5d5c0` 快进 |
| 标签 | `v0.8.13`（附注）→ `887d7f1` |
| index（0.8.13 与 latest 相同） | `sha256:f1dd072f9690b7b44933390c40c7400024360686b5e71001f84420e4b77c4281` |
| linux/amd64 | manifest `sha256:da9c51a7ea30e013535056cb25b38e174ef9367689402dfb3304d9dfd3d44064`，config `sha256:c4fc369667217cea3ce7011506b4ddb9ae98f4a07db869948ea346072b02cb5c` |
| linux/arm64 | manifest `sha256:c32d1e47dd4b090517d676294e6c8a21d85968eeeecbdc1d77602fb8f9f416b3`，config `sha256:7a82171929addf7356587bd7e208b8f30fbaa4734690a796add173594e9edf4f` |
| index 中另两项 | `unknown/unknown` 构建证明 |

## 5. Oracle1 **[生产只读]**

时间为 UTC。

| 项目 | 结果 |
| --- | --- |
| 部署前 | 17:03:59：进行中运行 0；未完成操作只有 2026-11-12 起的 1 项既有待核验（未改动）；下一计划运行 18:00 |
| 启动 | 2026-11-15T17:04:15Z |
| ImageID | `sha256:7a82171929ad…edf4f`，等于 arm64 config；RepoDigest 为上方 index |
| 版本 | healthz ok 0.8.13；readyz ready、0.8.13、schema 13 |
| 健康／重启 | running、healthy、重启 0 |
| 邮箱连接与同步 | 启动后 14/14 文件夹完成自动同步（最近 17:05:31）；启动后日志 error／panic 0 条、warn 0 条 |
| 页面资源 | 7 个文件（含新的 `tasks.js`、`style.css`、`index.html`）的 SHA-256 与源码相同 |
| 其他服务 | 23 个容器的重启次数与启动时间与部署前相同 |
| 自然任务 | 0.8.13 采集时（17:06）部署后没有运行，记为未核对；未等待、未触发。0.8.12 运行期间有 16:00:01、17:00:01 两次内置自然运行，均为 noop，不证明真实写入 |
| 生产任务 | 未在生产页面打开或保存任务，未调用模型、未运行任务、未写邮件 |

## 6. 只读发布核对与配置一致性 **[生产只读]**

命令：

```text
release_verify.py --remote origin --version 0.8.13 --commit 887d7f1 --ssh <Oracle1> --compose-dir <Compose 目录> --backup <…/upgrade-0.8.13-20261115-170359>
```

- 17:05:56–17:06:26 连续运行两次：**exit 3**（unverified），去掉时间后 JSON 完全相同。
- 12 项 match：标签、main（即发布提交）、latest 等于版本、双架构、服务器镜像、版本、健康、重启 0、文件夹 14/14、配置表 16/16（schema 13→13）、任务用户字段 4/4、主密钥一致（只输出布尔值）。
  - 16 张配置表包括规则、规则版本、模型连接、整理授权、通知、MCP 设置与令牌。
- 1 项未核对：`tasks.post_deploy_runs` 为 “no runs since start”。
- 本次采集时 `mcp_tokens` 与升级前备份相同。0.8.12 验收中出现过 OAuth 正常轮换导致的该表差异；那是运行中的自然变化，比较范围没有放宽。
- 部署文件：`compose.yaml`、`1panel.env` 与本次升级前备份逐字节相同，也与 0.8.12 升级前备份相同（单独 `cmp`）。
- 任务运行字段在采集时都未推进；部署后操作 `{}`；既有待核验操作 1，未改动。

只读证明：两次采集前后对 15 张表做行数与内容摘要的独立只读快照，只有 `task_runs` 一行不同。

- 那是 2026-11-12 的 needs_verification 手动运行，应用在 17:06:31 自行对账更新了 `updated_at`，状态不变。之前各版部署后同样出现过，属于历史遗留记录的自然对账，不是本版新增问题。
- 这期间审计 0 条，部署后操作 0 条。

## 7. 备份、回滚与清理

- 备份 `backups/upgrade-0.8.13-20261115-170359`（目录 700，数据库与主密钥 600）：一致性快照（integrity ok、schema 13）、主密钥（与在线密钥逐字节相同）、compose 与环境文件、其他服务清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.12`（保留）；未做回滚演练。发布记录 `releases/0.8.13.json`。
- 已删除：
  - `backups/upgrade-0.8.12-20261115-150321`：删除前确认在线主密钥、部署文件与其相同；
  - 镜像 `allen0039/mail-auto:0.8.11`：无容器使用。
- 保留：0.8.13／latest、回滚镜像 0.8.12、全部发布记录、部署配置、邮件数据与密钥。cloudflared 的悬空镜像不属于本项目，未处理。
- 服务器上没有留下临时脚本。本地隔离实例与临时目录由检查入口自动删除。

## 8. 已知限制与未核对

- 0.8.13 部署后尚无自然任务运行（下一计划 18:00 UTC），记为未核对。
- 新功能只在隔离实例和假接口中验证，没有在生产页面上操作任务（按要求）。界面布局只有自动检查：三个宽度下编辑器无横向溢出；没有另做人工截图检查。
- 规则选择的复选框旁仍显示加载时的“vN”。刷新后以下方版本区为准；版本区会随规则列表刷新更新。
- 既有行为未改：原绑定的规则被删除或不再适用于源目录时，不出现在复选框中，保存只提交仍可选的规则。本版在版本区明确提示，并且不提供确认操作。
- 历史遗留的 1 项待核验操作和对应运行未处理。

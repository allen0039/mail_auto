# 0.8.5 验收记录：UI 检查门禁 LG-01 补修

范围：只修复 0.8.4 独立审计 LG-01（P2）——结果校验可被空 `failed` 数组和非布尔 `ok` 绕过。只改测试工具；产品功能、后端与任务编辑器不变，数据库仍为 schema 13，无迁移。第二次交付（任务编辑器与错误提示）未开始。本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[模拟]** 隔离临时实例 + 假数据接口；**[生产只读]** Oracle1 只读查询；**[真实写入]** 原邮箱实际写入（本版没有）。

## 1. 修改范围

| 文件 | 修改 |
| --- | --- |
| `scripts/ui-check/run-checks.cjs` | `problems()`：`results` 与 `failed` 同时存在时两者都检查——`results` 中每个 `ok:false` 都计为失败，`failed` 条数须与之相符，否则判为不一致；逐项结果须为对象且 `ok` 为布尔值（字符串、数字、缺失均无效）；`failed` 条目须为带 `name` 的对象且 `ok` 若存在必须为 `false`；`total` 与 `results` 数量不符、失败多于总数均失败。`{pass, results}` 与 `{pass, total, failed}` 两种格式继续可用。导出 `problems` 供回归使用，仅在直接执行时运行浏览器 |
| `scripts/ui-check/gate-test.cjs`（新增） | `node --test` 门禁回归：18 个结果校验用例（含审计两个反例）始终运行；设置 `PUPPETEER` 时另有 9 个用例经 `run.sh` 端到端断言退出码，并核对每次运行后无临时目录或实例进程遗留 |
| `README.md` | 一句话说明如何运行门禁回归；版本号 |
| `CHANGELOG.md`、`compose.yaml`、`internal/version/version.go`、`internal/admin/web/index.html`、`docs/operations.md`（回滚示例版本） | 0.8.5 |

保留：页面异常、执行异常、零检查、普通失败的非零退出；`finally` 关闭浏览器；`run.sh` 的隔离实例、一次性凭据与 `trap` 清理；三套正式检查脚本与断言未改。

## 2. 测试

| 命令 | 结果 |
| --- | --- |
| `make check` | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.5` |
| `node --check`（全部页面与检查脚本）、`bash -n run.sh`、`git diff --check`、`docker compose config -q`、`gofmt -l` | 全部通过 |
| `node --test scripts/ui-check/gate-test.cjs`（不设 PUPPETEER） | 18 通过，9 跳过（端到端） |
| 同一回归套在 0.8.4 的 `problems()` 上 | 4 失败：两个 LG-01 反例、数字 `ok`、`total` 与 `results` 不符——回归能发现原缺陷 |
| `PUPPETEER=… node --test scripts/ui-check/gate-test.cjs` **[模拟]** | 27/27 通过，exit 0 |
| `run.sh`（默认三套）**[模拟]** ×2，`CHROME_PATH=<本机 Chrome> run.sh` ×1 | 均 exit 0；settings 45、model 63、ownership 77，1280 / 375 / 390 全部通过，页面错误 0 |

端到端门禁用例（每次一个隔离实例）：

| 用例 | 预期 | 实际 |
| --- | --- | --- |
| 合法 `{pass, results}` | 0 | 0 |
| 合法 `{pass, total, failed}` | 0 | 0 |
| LG-01：`results` 含 `ok:false`，`failed: []` | 非零 | 1 |
| LG-01：`ok: "false"` | 非零 | 1 |
| 普通断言失败 | 非零 | 1 |
| 缺少 `pass` | 非零 | 1 |
| 零检查 | 非零 | 1 |
| 页面异步抛错 | 非零 | 1 |
| 脚本 Promise reject | 非零 | 1 |

审计报告原注入文件直接运行：`masked-failure.js` exit 1（三个宽度均报 “failed list does not match results” 与 “pass is true but checks failed”）；`invalid-ok.js` exit 1（三个宽度均报 “invalid check #1: ok must be a boolean”）。运行中对 `run.sh` 发送 SIGTERM：exit 130。

清理：上述全部运行结束后，`$TMPDIR` 下 `mail-auto-ui-check.*` 与 `gate-fixture-*` 目录 0 个，headless Chrome 进程 0，临时实例进程 0。中断检查限于实测的 SIGTERM，不推广到 SIGKILL 或断电。

## 3. 标签、镜像与 Oracle1

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `f8e4ca350c5b6216220812c76b182693f4a4b184`，已推送到 `origin/main`（前一提交为 `007cdda`） |
| 标签 | `v0.8.5` → `f8e4ca3…`（附注标签；推送前确认远端没有同名标签） |
| CI | GitHub Actions `ci`：main 43687854800 与 tag `v0.8.5` 43687857247 均为 success |
| 镜像 index（`0.8.5` 与 `latest` 相同） | `sha256:558aed12ea138ebd23ac24f747f9c867ad5d2983eb0dea0013e4af31432f9068` |
| linux/amd64 | `sha256:2020441e46a9b2dfeb126d5a15793460f2f36582bcdae1a361b7b1d9e6dc708d`，config `sha256:03ece1be…a8b0` |
| linux/arm64 | `sha256:f7773efb07342144f848bfb9eeb3c6f300f771afb8ab1a72a2ff6fa559a8d977`，config `sha256:163ec0e6d5b2c18fb669389ddb7403bef2ef1edac35c2566d6e1163c3bfce917` |
| index 中另两项 | `unknown/unknown`，为 buildx 构建证明（attestation） |
| Oracle1 运行镜像 **[生产只读]** | ImageID `sha256:163ec0e6…e917`（arm64），与 arm64 config 一致；RepoDigest 为上方 index |
| 版本 / schema | `/healthz` 0.8.5 ok；`/readyz` 0.8.5、schema 13、ready |
| 线上页面 | `/` 与源码 `index.html`、以及 `app.js`、`automation.js`、`rules.js`、`tasks.js`、`organizer.js` 的 SHA-256 与源码相同 |
| 健康 / 重启 | healthy，重启 0，2026-11-15T07:07:05Z 启动 |
| 部署前检查 **[生产只读]** | 07:06:47Z：进行中运行 0；进行中写入操作 0（2026-11-12 起的 1 项既有待核验操作未改动）；下一个计划运行 08:00Z |
| 邮箱同步 **[生产只读]** | 07:09:33Z 一次 “等待邮箱服务器响应超时（45 秒）” 后自动重连；07:09:49–07:10:05Z 14 个文件夹全部完成同步 |
| 日志 | 截至 07:11:40Z，error / panic 0 条；WARN 仅上述 1 条 |
| 其他服务 | 23 个容器的 ID、重启次数、状态与启动时间均与部署前清单一致 |

**任务证据 [生产只读]**

- 本轮没有触发运行、没有修改任务，也没有等待自然调度。0.8.5 启动后（截至 07:11Z）尚无运行，也没有新的写入操作。
- 0.8.4 期间有一次自然调度：`6bcWR3_m…`，07:00:02Z，C-RpBG… 内置定时，noop，扫描 500、领取 0、失败 0、未知 0，通知 `not_required`。
- **noop 只说明任务能运行、当次没有可处理的邮件，不证明真实写入。**

## 4. 升级前后配置对比、备份与清理

- 与 0.8.4 相同的只读脚本比较 0.8.5 升级前快照与部署后数据库（07:11Z）：schema、16 张配置表内容摘要、任务字段、整理授权字段、当前模型配置，**20/20 相同**；运行期字段也相同。
- 与 0.8.4 升级前快照（06:21Z）相比：19/20 相同；唯一差异是任务 C-RpBG… 载荷中的调度运行状态 `last_triggered`、`cursor`、`next_run_at`，来自 07:00Z 的自然调度，用户配置字段（每批、保护、历史、通知、日程、启用、触发、版本）未变。
- 关键值不变：4 个任务均为内置定时 / 固定规则 / 执行、每批 20、保护未读与星标、历史起点 0、通知跟随；整理授权每批 20、保护未读与星标、包含历史、无每日额度；当前模型配置版本 5、2 个模型连接；邮箱 1、邮件 1819、文件夹 14、规则 10、任务 4、运行 125。
- 备份 `backups/upgrade-0.8.5-20261115-070657`（目录 700，数据库与主密钥 600）：一致性快照 integrity ok、quick_check ok、schema 13，主密钥、compose 与环境文件、其他 23 个服务基线清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.4`（已保留）；未做回滚演练。发布记录 `releases/0.8.5.json`。
- 清理：删除镜像 `allen0039/mail-auto:0.8.3`（无容器使用）与备份 `upgrade-0.8.4-20261115-062114`（已由 0.8.5 升级前备份取代）；保留 0.8.5 / latest、回滚镜像 0.8.4、全部发布记录、部署配置、邮件数据与密钥。悬空镜像只有 cloudflared 的一个，不属于本项目，未处理。Oracle1 上的临时只读脚本已删除。

## 5. 未验证与说明

- 门禁修复与回归均为 **[模拟]**（隔离临时实例、临时凭据、假数据接口）；没有使用生产账号或生产页面。
- 0.8.5 部署后尚无任务运行记录（下一次计划运行 08:00Z），未等待，列为未验证。
- 写入证据：本版没有新的真实写入，也没有触发模型调用；最近已核验的真实写入仍是 0.8.0 时期的固定规则任务；模型分类写入仍只有 **[用户确认]**。
- UI 检查仍未接入 Go CI（按方案作为前端发布前必跑项）。

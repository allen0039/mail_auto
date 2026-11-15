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

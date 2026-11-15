# 0.8.4 验收记录：验证入口与文案（轻量优化第一次交付）

范围：轻量优化方案的“建议一”（UI 检查成为可靠门禁）与“建议四”（重复文案与使用文档）。不含任务编辑器层级调整与任务错误提示（第二次交付）。数据库仍为 schema 13，无迁移；后端逻辑无改动。本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[模拟]** 隔离临时实例 + 假数据接口；**[生产只读]** Oracle1 只读查询；**[真实写入]** 原邮箱实际写入（本版没有）。

## 1. 修改范围

| 文件 | 修改 |
| --- | --- |
| `scripts/ui-check/run-checks.cjs` | 每个脚本结果先校验（须有布尔 `pass`、至少一项检查、可读的失败列表；`pass` 与逐项结果矛盾也算失败）；全部有效且通过、页面错误为 0 时 exit 0，否则 exit 1；执行异常打印一行原因并 exit 1；`finally` 关闭浏览器（关闭失败则强杀进程）。浏览器取 `CHROME_PATH` / `PUPPETEER_EXECUTABLE_PATH`，否则由 puppeteer 发现本机 Chrome（`channel: "chrome"`），不再写死 macOS 路径。凭据文件含临时引导账号时先在页面内完成首次设置 |
| `scripts/ui-check/run.sh`（新增） | 薄入口：临时目录构建二进制，随机端口启动隔离实例（`-automatic-sync=false`，数据/密钥/secret 目录均在临时目录），读取一次性引导凭据并生成随机新凭据（文件权限 600），默认运行 settings / model / ownership 三套检查；`trap` 在正常结束、失败、中断时停止实例并删除临时目录 |
| `internal/admin/web/index.html` | 删除「整理设置」中重复的一段说明；任务区与 Agent 页各补一句“按语义分类由外部 MCP 客户端执行 / 配置模型不会自动分类邮件”；版本号 |
| `README.md` | 开头改为当前能力 +“选择使用方式”三条路径表；历史账号数量、验证样本与逐版验收链接移出正文；升级说明区分同 schema 回滚与跨 schema 恢复；补充 UI 检查运行说明 |
| `docs/operations.md` | 「升级与回退」新增同 schema 小版本回滚与跨 schema 恢复对照表 |
| `docs/configuration.md` | 承接 README 移出的真实写入验证历史说明（内容不变） |
| `CHANGELOG.md`、`compose.yaml`、`internal/version/version.go` | 0.8.4 |

保留的行为：三套检查脚本内容与断言未改（45 / 63 / 77 项）；宽度仍为 1280 / 375 / 390；Go 代码、任务编辑器、账号切换与异步保护、草稿保护、就绪判断、旧运行链接、模型连接与输出长度参数的后端兼容均未改动。

## 2. 测试

| 命令 | 结果 |
| --- | --- |
| `make check`（race 测试、vet、gofmt、staticcheck@v0.8.2） | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.4` |
| `node --check`（全部页面脚本与检查脚本）、`bash -n run.sh`、`git diff --check`、`docker compose config -q` | 全部通过 |
| `run.sh`（默认 Chrome 发现）**[模拟]** | exit 0；settings 45、model 63、ownership 77，三个宽度全部通过，页面错误 0 |
| `CHROME_PATH=<本机 Chrome> run.sh` **[模拟]** | exit 0；同上 |

失败注入（每次单独运行 `run.sh <注入脚本>`，注入脚本只放在会话临时目录，不入库）：

| 注入 | 退出码 | 输出 |
| --- | --- | --- |
| 断言失败（`pass:false` + 一项失败） | 1 | `injected assertion: expected failure` |
| 页面脚本异常（`setTimeout` 中抛错，结果本身 pass） | 1 | `pageErrors` 含 `injected page error` |
| 执行异常（脚本 reject） | 1 | `UI check aborted: injected script exception` |
| 无效结果（无 `pass` 字段） | 1 | `invalid result: missing boolean pass` |
| 结果自相矛盾（`pass:true` 但有失败项） | 1 | `pass is true but checks failed` |
| 浏览器路径不存在 | 1 | `Browser was not found at the configured executablePath` |
| puppeteer 模块不存在 | 1 | `Cannot find module` |
| 运行中 SIGTERM | 130 | — |

每次运行后检查：`$TMPDIR` 下无 `mail-auto-ui-check.*` 目录、无 headless Chrome 进程、无临时实例进程，均为 0。

附带发现（未修改）：`admin-ui-check.js` 不在三套门禁内。用新入口运行时三个宽度均为 50/51，失败项为 “focus falls back to the record summary when its button is gone”；同一检查在 0.8.3 源码上结果完全相同，属既有问题，旧入口因不看结果而未暴露。`network-ui-check.js` 只允许在固定端口 18600 的代理验收实例上运行，脚本自身拒绝并使入口 exit 1。两者本版不处理。

## 3. 未验证与说明

- 本版只改文档、说明文案和检查入口；界面验证均为 **[模拟]**。
- UI 检查尚未接入 Go CI（方案约定先作为前端发布前必跑项）。

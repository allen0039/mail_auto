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

## 3. 标签、镜像与 Oracle1

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `40f0336b316ecb71219eacb176a27beae0cb5c08`，已推送到 `origin/main`（前一提交为 `9035565`） |
| 标签 | `v0.8.4` → `40f0336…`（附注标签；推送前确认远端没有同名标签） |
| CI | GitHub Actions `ci`：main 43684508338 与 tag `v0.8.4` 43684516902 均为 success |
| 镜像 index（`0.8.4` 与 `latest` 相同） | `sha256:6f3759585d6f6e2acaea2c482345aad094a9ffd8fb1efb989571735bdbef1dd4` |
| linux/amd64 | `sha256:ce78e9c9754d13288182fcdabf58b0705d46a64eccf3eb2a18cb88f463e2b532`，config `sha256:07ecb910…b1c0` |
| linux/arm64 | `sha256:90234f7ae43ace3c8370c86f81689f9bc93f413983f071ac6991f162649af301`，config `sha256:eb772acbfa5e652ad177ccdff34666abf3bc3173889eaabb01950f935549193b` |
| index 中另两项 | `unknown/unknown`，为 buildx 构建证明（attestation） |
| Oracle1 运行镜像 **[生产只读]** | ImageID `sha256:eb772acb…193b`（arm64），与 arm64 config 一致；RepoDigest 为上方 index |
| 版本 / schema | `/healthz` 0.8.4 ok；`/readyz` 0.8.4、schema 13、ready |
| 线上页面 | `/` 与源码 `index.html` SHA-256 相同（重复说明 1 处、版本号 v0.8.4）；`app.js`、`automation.js`、`rules.js`、`tasks.js`、`organizer.js` 与源码相同 |
| 健康 / 重启 | healthy，重启 0，2026-11-15T06:21:27Z 启动 |
| 部署前检查 **[生产只读]** | 06:20:52Z：进行中运行 0；进行中写入操作 0（另有 1 项 2026-11-12 起的既有待核验操作，未改动）；下一个计划运行 07:00Z |
| 邮箱同步 **[生产只读]** | 06:22:46Z 一次 “等待邮箱服务器响应超时（45 秒）” 后 5 秒自动重连；06:23:02–06:23:21Z 14 个文件夹全部完成同步 |
| 日志 | 截至 06:26Z，error / panic 0 条；WARN 仅上述 1 条 |
| 其他服务 | 23 个容器的 ID、重启次数、状态与启动时间均与部署前清单一致 |

**任务证据 [生产只读]**（截至 06:26Z）

- 本轮没有触发运行、没有修改任务，也没有等待自然调度。0.8.4 启动后尚无运行，也没有新的写入操作。
- 0.8.3 期间最后一次运行：`jiZLw6df…`，06:00:04Z，C-RpBG… 内置定时，noop，扫描 259、领取 0、失败 0，通知 `not_required`。
- **noop 只说明任务能运行、当次没有可处理的邮件，不证明真实写入。**

## 4. 升级前后配置对比、备份与清理

- 方法：同一只读脚本分别读取升级前快照与部署后数据库，比较 20 项：schema、16 张配置表的内容摘要（任务、整理授权、账号（密钥只取摘要）、文件夹设置、规则与规则版本、通知通道、Telegram 交互与绑定、网络设置、代理、账号网络策略、Agent 授权、当前模型配置、模型连接、MCP 设置），以及任务字段、整理授权字段和当前模型配置。**结果 20/20 相同**；运行期字段（任务下次运行时间、Telegram 轮询状态）也相同。
- 关键值（前后相同）：4 个任务均为内置定时 / 固定规则 / 执行、每批 20、保护未读与星标、历史起点 0、通知跟随；整理授权每批 20、保护未读与星标、包含历史、无每日额度；当前模型配置版本 5、2 个模型连接；schema 13。
- 计数（前后相同）：邮箱 1、邮件 1819、文件夹 14、规则 10、任务 4、运行 124、模型连接 2。
- 备份 `backups/upgrade-0.8.4-20261115-062114`（目录权限 700）：一致性数据库快照（integrity ok、quick_check ok、schema 13）、主密钥、compose 与环境文件、其他 23 个服务的基线清单。
- 回滚：同 schema 小版本，可直接改回 `allen0039/mail-auto:0.8.3`（已保留）。本次未做回滚演练。发布记录 `releases/0.8.4.json`。
- 清理：删除镜像 `allen0039/mail-auto:0.8.2`（无容器使用）与备份 `upgrade-0.8.3-20261115-052156`（已由 0.8.4 升级前备份取代）；保留 0.8.4 / latest、回滚镜像 0.8.3、全部发布记录、部署配置、邮件数据与密钥。主机上唯一的悬空镜像属于 cloudflared，未处理。Oracle1 上的临时只读脚本已删除。本地临时实例、凭据与目录由检查入口自动删除，复核为 0。

## 5. 未验证与说明

- 本版只改文档、说明文案和检查入口；界面验证均为 **[模拟]**，执行者没有在生产页面上操作。
- 0.8.4 部署后还没有任务运行记录（下一次计划运行 07:00Z），未等待，列为未验证。
- 写入证据：本版没有新的真实写入；最近已核验的真实写入仍是 0.8.0 时期的固定规则任务；模型分类写入仍只有 **[用户确认]**。
- UI 检查尚未接入 Go CI（方案约定先作为前端发布前必跑项）。

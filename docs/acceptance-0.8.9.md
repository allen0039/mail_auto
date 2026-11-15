# 0.8.9 验收记录：UI 检查接入 CI、只读发布核对入口

范围：轻量优化交付一（A、B）。不实施 C（更新任务绑定规则版本），不新增产品功能。

- 产品代码、界面行为、数据库（schema 13，无迁移）不变；只改 CI、检查依赖、新增脚本、文档与版本号。
- `run.sh`、`run-checks.cjs`、`gate-test.cjs` 与四套检查脚本未改动；没有删除、放宽或跳过断言。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[CI]** GitHub Actions；**[本地]** 本机隔离实例；**[夹具]** 临时 SQLite 与假命令；**[生产只读]** Oracle1 只读查询。

## 1. A：UI 检查接入 CI

- 位置：`.github/workflows/ci.yml` 的 `ui` job，与原 `check` job 并行；触发不变（每次 push 与 PR）。原 Go 检查步骤全部保留，仅追加一步夹具测试。
- 依赖：`scripts/ui-check/package.json`／`package-lock.json` 固定 `puppeteer-core` 25.16.0（含 `@puppeteer/browsers` 3.2.6）；Node 24.14.0；浏览器为 Chrome for Testing 156.0.8078.47（该 puppeteer 版本对应的构建，由 `@puppeteer/browsers` 安装到 `$RUNNER_TEMP`，`CHROME_PATH` 指向它）。`.dockerignore` 不含 `scripts/`，运行镜像不变。
- 步骤：`npm ci` → 安装 Chrome → `scripts/ui-check/run.sh`（默认四套、三个宽度）→ 27 项门禁（TAP 输出须为 `# pass 27` 与 `# skipped 0`）→ `if: always()` 核对没有遗留 `mail-auto-ui-check.*`、`gate-fixture-*` 目录或实例进程。
- 不上传产物；日志只有检查名称与计数。
- 本地用法：`npm ci --prefix scripts/ui-check && scripts/ui-check/run.sh`（README 已更新）。

| 运行 | 提交 | 结果 |
| --- | --- | --- |
| 分支 [43707604694](https://github.com/allen0039/mail_auto/actions/runs/43707604694) **[CI]** | `e0fe45a` | success。`ui` 实际执行：Chrome for Testing 156.0.8078.47；每个宽度 settings 45、model 63、ownership 87、task-editor 68，共 12 组全部通过，页面错误 0；门禁 tests 27／pass 27／fail 0／skipped 0；遗留目录 0、进程 0。`check`：Go 全部步骤 + 夹具 16 项通过 |
| main [43708309374](https://github.com/allen0039/mail_auto/actions/runs/43708309374) **[CI]** | `e0fe45a` | success（check、ui） |
| tag v0.8.9 [43708319633](https://github.com/allen0039/mail_auto/actions/runs/43708319633) **[CI]** | `e0fe45a` | success（check、ui） |

失败注入（临时分支，各在 `e0fe45a` 上给 `settings-ui-check.js` 加一行，验证后已删除分支，未进入 main）：

| 注入 | 运行 | `ui` job | 失败原因（日志） |
| --- | --- | --- | --- |
| 断言失败 `check(..., false)` | [43707627434](https://github.com/allen0039/mail_auto/actions/runs/43707627434) | failure | UI 检查步骤非零；各宽度 `pass: false`，失败项 `CI-INJECT assertion failure` |
| pageerror（`setTimeout` 抛错） | [43707630953](https://github.com/allen0039/mail_auto/actions/runs/43707630953) | failure | `pageErrors` 含三次 `CI-INJECT page error`，exit 1 |
| 脚本异常（`throw`） | [43707634076](https://github.com/allen0039/mail_auto/actions/runs/43707634076) | failure | `UI check aborted: CI-INJECT script exception`，exit 1 |

三次注入中 `check` job 均成功（与 UI 独立），清理步骤均为遗留 0。四份日志检索 `Temporary password`、凭据字段均为 0 处。

## 2. B：只读发布核对入口

- 位置：`scripts/release-verify/release_verify.py`；夹具测试 `test_release_verify.py`；用法、字段、退出码见 `docs/operations.md`「发布后只读核对」。
- 参数：`--version`、`--commit`、`--ssh`、`--compose-dir`、`--backup`（必填）；`--image`、`--service`、`--remote`、`--repo`（可选）。脚本不含任何主机、路径或凭据。
- 必检项（13）：`git.tag`、`git.main`、`image.latest_is_version`、`image.platforms`、`server.image`、`server.version`、`server.health`、`server.restarts`、`sync.folders`、`tasks.post_deploy_runs`、`config.tables`、`tasks.user_fields`、`master_key`。每项为 match／mismatch／unverified，附简短说明。
- 退出码：0 全部匹配；1 有不一致；3 无不一致但有未核对；2 参数错误。
- 只读手段：git `rev-parse`／`ls-remote`／`cat-file`／`merge-base`／`diff`；`docker buildx imagetools inspect`；服务器 `docker ps`／`inspect`／`image inspect`、`curl` 健康接口、SQLite `mode=ro` + `query_only` 并在只读事务中回滚；主密钥在服务器内逐字节比较，只输出布尔值。

夹具测试 **[夹具]**：`python3 -m unittest discover -s scripts/release-verify -v` → 16 项通过（本机 Python 3.9；CI 中同样通过）。

| 用例 | 断言 |
| --- | --- |
| 正常匹配 | exit 0，13 项 match；连续两次结果除时间外相同；运行前后数据库与密钥文件内容和 mtime 不变；只发出白名单内的只读命令 |
| 脱敏 | 输出不含密钥、密钥 SHA-256、service_owner、文件夹名、备份完整路径、SSH 目标、Compose 路径、配置内容 |
| 版本／摘要不一致 | latest 与版本摘要不同、健康版本不同、运行镜像不同 → exit 1，三项 mismatch |
| 标签指向其他提交、重启 | `git.tag`、`git.main`、`server.restarts` mismatch |
| 配置不一致 | 修改规则与任务每批数量 → `config.tables`（列 payload）、`tasks.user_fields` mismatch，不输出新值 |
| 主密钥不同 | `master_key` mismatch |
| 缺备份 | exit 3；配置、任务、主密钥三项 unverified，不会 match |
| 数据库不可读 | 同步、运行、配置、任务 unverified |
| 命令失败 | ssh、buildx、git、curl、docker inspect 分别失败 → 对应项 unverified，exit 3 |
| 无部署后运行 | 立即 exit 3，`tasks.post_deploy_runs` 为 “no runs since start” |
| 运行分类 | noop、成功 → match（只有 noop 时注明不是真实写入证明）；失败 → mismatch；待核验、进行中 → unverified |
| 自然任务推进 | cursor、last_triggered、next_run_at、updated_at 推进时用户字段仍 match，变化列在 `task_runtime_fields` |
| 文件夹未同步完 | `sync.folders` unverified |
| main 之后的提交 | 只有文档 → match 并列出文件；有代码 → mismatch |
| 不一致优先 | 同时有不一致和缺证据 → exit 1 |

上线前对 0.8.8 生产的只读试运行 **[生产只读]**：exit 0，13/13 match；任务 C-RpBG… 的 cursor／last_triggered／next_run_at 推进被单列，用户字段 4/4 相同。

## 3. 本地验证

| 命令 | 结果 |
| --- | --- |
| `make check`（race 测试、vet、gofmt、staticcheck） | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.9` |
| `node --check`（页面与检查脚本）、`bash -n run.sh`、`py_compile`、`docker compose config -q`、`git diff --check`、`gofmt -l` | 全部通过 |
| `CHROME_PATH=<Chrome for Testing 156.0.8078.47> scripts/ui-check/run.sh` **[本地]** ×2 | exit 0；四套 45/63/87/68，三个宽度，页面错误 0 |
| `PUPPETEER=puppeteer-core node --test scripts/ui-check/gate-test.cjs` **[本地]** | 27/27，fail 0，skipped 0 |
| 运行后资源 | 无 `mail-auto-ui-check.*`、`gate-fixture-*`、临时实例或 Chrome 进程 |

LE-01、LE-02 在 task-editor 68 项中，DP-01 在 ownership 87 项中，本地与 CI 均通过。

## 4. 发布、镜像与 Oracle1

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `e0fe45a8228e9390099c13d6f46393dbdf8870ca`，main 由 `ac68ace` 快进 |
| 标签 | `v0.8.9`（附注）→ `e0fe45a` |
| 镜像 index（0.8.9 与 latest 相同） | `sha256:52045a82058fe9969fa153e6809a887a2382712d73f27076485642b024cb49b1` |
| linux/amd64 | manifest `sha256:590c4c13…e98a`，config `sha256:7991d6d0c0b5c22be2692aa519246120dd9ffd2ba5bc46c181fb486259b0e3b0` |
| linux/arm64 | manifest `sha256:f43f8510…7a80`，config `sha256:7c125171190fee6994b9d2c9d589e287b38030fbcf5e859b7d02705c857a599f` |
| index 中另两项 | `unknown/unknown` 构建证明 |
| Oracle1 **[生产只读]** | 11:26:27Z 启动；ImageID 等于 arm64 config；RepoDigest 为上方 index；healthz／readyz 0.8.9、schema 13、ready；healthy；重启 0 |
| 部署前 | 进行中运行 0；未完成操作只有 2026-11-12 起的 1 项既有待核验（未改动）；下一计划运行 12:00Z |
| 页面资源 | index.html、app.js、automation.js、rules.js、tasks.js、organizer.js、style.css 与源码 SHA-256 相同 |
| 日志 | 启动后 error／panic 0 条 |
| 其他服务 | 23 个容器的重启次数与启动时间与部署前相同 |

## 5. 新入口重新采集的结果 **[生产只读]**

命令（11:28Z，连续两次）：`release_verify.py --version 0.8.9 --commit e0fe45a --ssh <Oracle1> --compose-dir <Compose 目录> --backup <…/upgrade-0.8.9-20261115-112013>`

- 两次均 exit 3，结论 unverified；去掉时间后两次 JSON 完全相同。
- 12 项 match：标签与 main（main 即发布提交）、latest＝版本、双架构、服务器镜像、版本、健康、重启 0、文件夹 14/14 于 11:26:51Z 前完成同步、配置表 16/16（schema 13→13）、任务用户字段 4/4、主密钥一致。
- 1 项 unverified：`tasks.post_deploy_runs` “no runs since start”。部署后尚无自然运行，未等待、未触发。
- 4 个任务的运行字段在采集时都未变化；部署后新增操作 0；既有待核验操作 1（未改动）。

只读证明：两次采集前后对 15 张表做行数与内容摘要的独立只读快照。只有 `task_runs` 一行不同：2026-11-12 的一次手动运行（状态 needs_verification）在 11:28:47Z 被应用自身对账更新了 `updated_at`，状态未变，没有审计或操作变化。核对入口的连接为 `mode=ro` + `query_only`，不能写入；这是应用的自然变化，单独列出。

本文件提交后 main 只多出文档提交，再次运行时 `git.main` 应报告“adds only documentation”。

## 6. 备份、回滚与清理

- 备份 `backups/upgrade-0.8.9-20261115-112013`（目录 700，数据库与主密钥 600）：一致性快照（integrity ok、schema 13）、主密钥（与在线密钥逐字节相同）、compose 与环境文件、其他服务清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.8`（保留）；未做回滚演练。发布记录 `releases/0.8.9.json`。
- 已删除：`backups/upgrade-0.8.8-20261115-094903`（删除前确认在线主密钥与其相同）；镜像 `allen0039/mail-auto:0.8.7`（无容器使用）。
- 保留：0.8.9／latest、回滚镜像 0.8.8、发布记录、部署配置、邮件数据与密钥。唯一的悬空镜像属于 cloudflared，未处理。
- 服务器上没有留下临时脚本（全部经 SSH 标准输入执行）。
- 本地：临时实例与目录由检查入口自动删除；Chrome for Testing 只下载在会话临时目录；`scripts/ui-check/node_modules` 已加入 `.gitignore`。

## 7. 未验证与说明

- 0.8.9 部署后尚无任务运行（下一计划 12:00Z），未等待，列为未核对。本版没有真实写入，也没有模型调用。
- 核对 Oracle1 期间 SSH 出现数次 “Connection closed”，重试后成功。核对入口在 SSH 失败时会把服务器项标为未核对（夹具已覆盖）。
- CI 的 Chrome 需在 Ubuntu 24.04 上放开 `kernel.apparmor_restrict_unprivileged_userns`（只作用于临时 runner），`run-checks.cjs` 的启动参数未改，仍启用沙箱。
- 核对入口不检查 CI 状态与日志，也不替代独立审计。

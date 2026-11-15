# 0.8.10 验收记录：只读发布核对入口 RV-01～RV-03 修复

范围：只修 0.8.9 独立审计的 RV-01、RV-02、RV-03（P2，均在 `scripts/release-verify/release_verify.py`）。

- 产品代码、界面、CI 工作流、UI 检查脚本与门禁不变；数据库仍为 schema 13，无迁移；没有新增依赖。
- 不实施原方案第三项（更新任务绑定规则版本），不新增功能。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[夹具]** 临时 SQLite、假外部命令或真实子进程；**[本地]** 本机隔离实例；**[CI]** GitHub Actions；**[生产只读]** Oracle1 只读查询。

## 1. 根因、修改与回归

### RV-01 未收尾或未知状态的任务被判为通过

**根因**

- 采集任务时只取了状态和计数，没有取已有的 `finalized`、`queued`、`running`。
- `classify_run` 把 cancelled 等状态归为 `other:*`；`evaluate` 只排除 failed、running、needs_verification，其余非空结果默认为 match。
- 服务在取消时保存 `cancelled` + `finalized=false`（`internal/mail/task_runs.go:438`），这是合法的中间状态，却被当成通过。

**修改**（`release_verify.py` 的 `classify_run`、`count`、`collect_remote` 的运行采集、`evaluate` 的任务项）

- 进行中状态（queued、running、awaiting_ai）先判为 running。
- failed、partial、interrupted 或 `failed>0` 判为 failed，对应 mismatch。
- needs_verification 或 `unknown>0` 判为 needs_verification。
- `finalized` 不是布尔值，或计数不是非负整数（含载荷不是 JSON 对象），判为 missing_evidence。
- `finalized=false`，或仍有 queued／running 写入，判为 not_finalized。
- 已收尾时按状态映射：noop、completed→succeeded、observed、cancelled、limited、requires_ai；其他状态为 unknown_state。
- 只有 noop、succeeded、observed 算通过，其余类别都是未核对。部署后仍有 queued／running 的写入操作时也是未核对。
- 输出每次运行的 `finalized` 与 queued／running 计数；`operations_since_start` 改为按状态计数。
- 任务运行逻辑和数据库都没改。

**回归**：`RunEvidenceTest` **[夹具]**

- 审计复现：cancelled + `finalized=false` + `running=1` + 部署后一条 running 操作，结果 exit 3、unverified，说明为 “not_finalized 1; 1 operation(s) since start still queued or running”。
- 13 种未核对情况：
  - 未知状态；
  - completed 未收尾，或有 queued 写入；
  - 缺 `finalized`，或 `finalized` 为字符串；
  - 计数为字符串，载荷不是 JSON 或是数组；
  - cancelled、limited、requires_ai；
  - awaiting_ai；
  - needs_verification。
- 已收尾的正常与失败结果：
  - noop、completed、observed 为 match；其中 noop 和 observed 注明不是真实写入证明；
  - failed、partial、interrupted 为 mismatch。
- 部署后已完成的操作不阻塞判断；正常运行与未收尾运行同时存在时为未核对。
- 审计探针 `independent_cli_probes.py` 对修复后代码重跑：cancelled 未收尾、cancelled 已收尾、未知状态、completed 未收尾均为 exit 3；observed 为 exit 0。

### RV-02 命令失败路径透传原始错误

**根因**

- `run` 把命令 stderr 的最后一行拼进异常；`short` 只截断，不脱敏。
- 采集器把这些文字写入 JSON 和检查说明，结论又打印到 stderr。

**修改**

- `run` 改抛 `SafeError`，只含命令名（`label`，限 git／docker／ssh／curl 及其子命令，其他一律记为 `command`）与退出码，或“timed out”“not found”“could not start (类型)”。命令 stderr 一律丢弃。
- `short` 只输出 SafeError 自身文本或固定类别：missing field、invalid data (类型)、permission denied、file not found、sqlite error (类型)、error (类型)。
- 本机收到服务器 JSON 后用 `scrub` 检查所有 `*error` 字段，不符合 `[A-Za-z0-9_ .,()-]{1,120}` 的改为 `error (details withheld)`。
- 各采集段统一捕获异常。服务器端未预期崩溃时只返回 “remote collection failed: error (类型)”；本机未预期错误只打印类别，按未核对 exit 3 退出。

**回归**：`RedactionTest` **[夹具]**

- 用会把含口令的 URL、私有路径和 “Traceback” 写进 stderr 的假 ssh／git／docker／curl 可执行文件，走真实 `rv.run`／`subprocess`。四种都 exit 3，输出只有类似 “ssh exited 128”“git rev-parse exited 128” 的文字，不含任何标记。
- 服务器端 docker 失败时 container 只记 “docker ps exited 128”。
- 超时（“command timed out”）、命令不存在、6 种异常的原文都不进入 `short` 的结果。
- 主密钥文件不可读时为 “permission denied”，不含路径。
- 服务器端崩溃与异常错误文字被替换。
- 审计探针重跑：`private_marker_in_stdout_or_stderr=false`，server_error 为 “command exited 255”。

### RV-03 `--remote` 放首位时进入内部模式并崩溃

**根因**：公开选项 `--remote`（Git 远端）与服务器端采集的分派标识相同，`main` 在 argparse 之前只看第一个参数。

**修改**

- 服务器端采集改用内部标记 `--internal-remote-collect`（`REMOTE_MODE`），并由 `remote_main` 校验载荷：必须正好是 compose_dir、backup、service 三个非空字符串字段，否则打印 “invalid internal collection parameters” 并 exit 2。
- 公开参数全部交给 argparse，`allow_abbrev=False`。`--version` 须为 X.Y.Z，`--commit` 须为 4–40 位十六进制，否则报用法错误 exit 2。

**回归**：`ArgumentTest` **[夹具]**

- `--remote origin` 放首位、末位、写成 `--remote=origin`、放在中间改为 upstream，都正常得到 match；实际 ls-remote 使用了 upstream。
- 13 种错误输入经真实子进程运行，均 exit 2、标准输出为空、无 traceback：
  - 缺值的 `--remote`（首位或末位）；
  - 缺少必填参数；
  - 版本或提交格式错误；
  - 公开调用中混入内部标记；
  - 缩写参数；
  - 内部载荷缺失、不是 JSON、缺字段、多字段、类型错误、多余参数。
- 审计探针重跑：`--remote origin` 首位时输出 JSON 报告（探针在真实环境找不到假 SSH 目标，按设计为未核对 exit 3），不再抛 JSONDecodeError。

夹具测试合计 16 → 28 项。原 16 项只改了两处：
- 夹具运行补上 `finalized`／queued／running 字段；
- 失败运行不再附加“不是真实写入证明”说明。

其余断言未删除或放宽。

## 2. 测试

| 命令 | 结果 |
| --- | --- |
| `python3 -m unittest discover -s scripts/release-verify` **[夹具]** | Ran 28，OK |
| `make check`（race 测试、vet、gofmt、staticcheck） | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.10` |
| `node --check`（页面与检查脚本）、`bash -n run.sh`、`py_compile`、`docker compose config -q`、`git diff --check` | 全部通过 |
| `scripts/ui-check/run.sh`（Chrome for Testing 156.0.8078.47）**[本地]** | exit 0；每个宽度（1280／375／390）settings 45、model 63、ownership 87、task-editor 68，页面错误 0 |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` **[本地]** | tests 27，pass 27，fail 0，skipped 0 |
| 审计探针 `task-edge-check.js`（LE-01／LE-02）、`account-close-check.js`（DP-01）**[本地]** | exit 0；三个宽度分别 12/12、6/6 |
| 运行后资源 | 无 `mail-auto-ui-check.*`、`gate-fixture-*` 或实例进程 |

**真实 CI [CI]**

| 运行 | 提交 | 结果 |
| --- | --- | --- |
| 分支 [43712856506](https://github.com/allen0039/mail_auto/actions/runs/43712856506) | `c65f199` | check、ui 成功；夹具 Ran 28；门禁 pass 27、skipped 0；遗留 0 |
| main [43713617636](https://github.com/allen0039/mail_auto/actions/runs/43713617636) | `c65f199` | check=success，ui=success |
| tag v0.8.10 [43713620908](https://github.com/allen0039/mail_auto/actions/runs/43713620908) | `c65f199` | check=success，ui=success |

## 3. 发布与镜像

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `c65f19996fce757b33a1a46ca247c6058bcb7406`，main 由 `caf4a64` 快进 |
| 标签 | `v0.8.10`（附注）→ `c65f199` |
| index（0.8.10 与 latest 相同） | `sha256:a29b7a6b4b8f4fb3288ea9cf33e292880789bb1a1105d2a588bf9e1e33d946f1` |
| linux/amd64 | manifest `sha256:ef172586d809ffb6dc31468b25e31a3ff2c9b80cdf443a018188ba9136196b5e`，config `sha256:288f81aa0cb4ff822ea39c3a2a6487e329836fbac4c4ab7059b1f0427e8b37a5` |
| linux/arm64 | manifest `sha256:069cd6c088a37e5d2e4822f1f894102814647ac449d576757ad36794b428093a`，config `sha256:0c5daebaebce6d47ee7042bda6e6cca02b9d3aa4fbd4c808b2c56e1af910a1dc` |
| index 中另两项 | `unknown/unknown` 构建证明 |

## 4. Oracle1 **[生产只读]**

| 项目 | 结果 |
| --- | --- |
| 部署前 | 12:31:59Z：进行中运行 0；未完成操作只有 2026-11-12 起的 1 项既有待核验（未改动）；下一计划运行 13:00Z |
| 启动 | 2026-11-15T12:32:15Z |
| ImageID | `sha256:0c5daebaebce…10a1dc`，等于 arm64 config；RepoDigest 为上方 index |
| 版本 | healthz ok 0.8.10；readyz ready、0.8.10、schema 13 |
| 健康／重启 | running、healthy、重启 0 |
| 文件夹同步 | 12:33 首次采集时 0/14（服务启动后还未完成首次同步，入口正确标为未核对）；12:38:44Z 14/14 完成，复采为 match |
| 页面资源 | 7 个文件的 SHA-256 与源码相同 |
| 日志 | 启动后 error／panic 0 条 |
| 其他服务 | 23 个容器的重启次数与启动时间与部署前相同 |

## 5. 新入口采集结果与一致性 **[生产只读]**

命令（`--remote origin` 放在首位）：

```text
release_verify.py --remote origin --version 0.8.10 --commit c65f199 --ssh <Oracle1> --compose-dir <Compose 目录> --backup <…/upgrade-0.8.10-20261115-123159>
```

- 12:32:42–12:33:10Z 连续运行两次：都 exit 3，去掉时间后 JSON 完全相同。
  - 11 项 match；
  - 文件夹同步未核对（尚未完成首次同步）；
  - 部署后任务未核对（no runs since start）。
- 12:39Z 同步完成后再运行：exit 3。
  - 12 项 match：标签、main、latest 等于版本、双架构、服务器镜像、版本、健康、重启 0、文件夹 14/14、配置表 16/16（schema 13→13）、任务用户字段 4/4、主密钥一致；
  - 1 项未核对：部署后任务 “no runs since start”。部署后 13:00Z 才有计划运行，未等待，也未触发。
- 部署配置：`compose.yaml` 与 `1panel.env` 和本次升级前备份逐字节相同，也和 0.8.9 升级前备份相同（单独 `cmp`；入口本身不比较部署文件）。
- 4 个任务的运行字段在采集时都未推进；部署后新增操作 0；既有待核验操作 1。

只读证明：

- 两次采集前后对 15 张表做行数与内容摘要的独立只读快照，只有 `task_runs` 一行不同。
- 那是 2026-11-12 的手动运行 `WfT5wXFL…`（needs_verification），应用在启动约 1 分钟后（12:33:16Z）自行对账更新了 `updated_at`，状态不变，审计 0 条。0.8.9 部署后同样出现过（11:28:47Z）。
- 入口连接为 `mode=ro` + `query_only`，这是应用的自然变化。

## 6. 备份、回滚与清理

- 备份 `backups/upgrade-0.8.10-20261115-123159`（目录 700，数据库与主密钥 600）：一致性快照（integrity ok、schema 13）、主密钥（与在线密钥逐字节相同）、compose 与环境文件、其他服务清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.9`（保留）；未做回滚演练。发布记录 `releases/0.8.10.json`。
- 已删除：
  - `backups/upgrade-0.8.9-20261115-112013`：删除前确认在线主密钥与其相同、部署文件与其相同；
  - 镜像 `allen0039/mail-auto:0.8.8`：无容器使用。
- 保留：0.8.10／latest、回滚镜像 0.8.9、全部发布记录、部署配置、邮件数据与密钥。cloudflared 的悬空镜像不属于本项目，未处理。
- 服务器上没有留下临时脚本（全部经 SSH 标准输入执行）。本地临时实例与目录由检查入口自动删除。

## 7. 已知限制与未核对

- 0.8.10 部署后尚无任务运行（下一计划 13:00Z），记为未核对，未等待、未触发。本版没有真实写入，也没有模型调用。
- “已收尾”的判断依赖服务写入运行摘要的 `finalized` 与计数；入口不读取逐项操作明细。
- 入口不比较部署文件、不检查 CI 状态与日志、不替代独立审计。
- 核对 Oracle1 时 SSH 偶尔断开，重试后成功；SSH 失败时入口把服务器项标为未核对。

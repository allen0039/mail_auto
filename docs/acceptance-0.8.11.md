# 0.8.11 验收记录：只读发布核对入口 RV-04 修复

范围：只修 0.8.10 独立复验的 RV-04（P2，`scripts/release-verify/release_verify.py` 的任务证据完整性）。

- 产品代码、界面、CI 工作流、UI 检查与门禁不变；数据库仍为 schema 13，无迁移；没有新增依赖。
- 不实施原方案第三项。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[夹具]** 临时 SQLite、假外部命令或真实子进程；**[本地]** 本机隔离实例；**[CI]** GitHub Actions；**[生产只读]** Oracle1 只读查询。

## 1. RV-04

### 根因

1. `count(payload, key)` 使用 `payload.get(key, 0)`，缺失字段被当成 0。只有 `{"finalized": true}` 的 noop 运行因此判为通过。
2. 同步、运行记录、操作状态、部署后操作在同一个 try 里依次查询。后面的查询失败时只记 `database_error`，`evaluate` 又用 `(srv.get("operations_since_start") or {})` 把缺失结果当作空集合，任务项仍可判为 match。

### 修改（`release_verify.py`）

**`REQUIRED_COUNTS` 与 `count`**

- 必需字段为 failed、unknown、queued、running，以及布尔值 `finalized`。
- 字段缺失、或不是非负整数（含字符串、负数、浮点、布尔），`count` 返回 None，判为 `missing_evidence`，不再默认 0。

**`classify_run`**

- 由状态本身确定的含义先判断，不受计数缺失影响：
  - queued、running、awaiting_ai → running（未核对）；
  - failed、partial、interrupted → failed（不一致）；
  - needs_verification → 待核验（未核对）。
- 其余状态先检查必需字段是否完整，完整后再按 RV-01 规则判断：
  - `failed>0` → failed；`unknown>0` → 待核验；
  - 未收尾或仍有 queued、running → not_finalized；
  - 已收尾时：noop、completed、observed 通过；cancelled、limited、requires_ai 和未知状态为未核对。

**分段采集（`section`、`sync_stats`、`post_deploy_runs`）**

- sync、runs、open_operations、operations_since_start 各自单独查询。
- 某段失败时不写入该段结果，只记录 `<段名>_error`（脱敏类别，如 `sqlite error (OperationalError)`），JSON 证据中一并列出。

**`evaluate` 任务项**

- runs 缺失 → 未核对，说明为 “runs not collected: <原因>”。
- `open_operations` 或 `operations_since_start` 缺失、失败或不是对象 → 未核对，说明为 “<段名> not collected: <原因>”。
- 正常查询得到 `{}` 仍视为空。
- 任一查询中仍有 queued 或 running 操作 → 未核对。
- 失败运行仍为不一致；整体结果仍是不一致优先于未核对。

### 兼容条件

**不设兼容例外**，缺失字段一律不按 0 处理。依据：

- `internal/mail/task_runs.go` 中 TaskRun 的 `finalized`、`failed`、`unknown`、`queued`、`running` 都没有 omitempty，当前二进制写出的每条运行都含这五个字段。
- **[生产只读]** 生产库 131 条运行中：
  - 35 条缺 queued、running，全部创建于 0.4.18 之前，最晚 2026-11-12 12:00Z；
  - 其余 96 条五个字段齐全。
- 入口只评估容器启动之后创建的运行。它们一定由当前部署的二进制写出，不可能是缺字段的旧记录。

### 回归（`EvidenceCompletenessTest`，9 项）**[夹具]**

| 用例 | 结果 |
| --- | --- |
| noop、completed、observed 各自逐项缺 failed／unknown／queued／running／finalized，或填入 -1、"0"、0.0、true（finalized 为 "true"、1） | 全部 exit 3、unverified、`missing_evidence 1` |
| 审计复现一：只有 `{"finalized": true}` 的 noop | exit 3 |
| 有效零值：noop、observed、completed 字段齐全且为 0 | exit 0、match；证据中 queued=0 |
| 无计数时状态含义保持：failed、partial → mismatch（exit 1）；needs_verification、running → unverified；completed 加 failed=2 → failed，加 unknown=1 → needs_verification | 通过 |
| 审计复现二：部署后一条 running 操作，`operations` 改为只含 id、state 的视图 | open_operations 采到 running=1，operations_since_start 为 null；报告 `sqlite error (OperationalError)`；exit 3，不再 match |
| 两项操作查询都失败（删除表） | exit 3，两项都写明 not collected，不含 “no such table” 原文 |
| 运行记录与同步查询失败 | runs 写明 “runs not collected: sqlite error (OperationalError)”；sync 未核对 |
| 服务器结果缺失：两项操作结果分别被删除、为 null、为数组、为字符串 | exit 3，“<段名> not collected: no data” |
| 正常空结果与优先级：noop + 空操作 → exit 0；失败运行 + 操作表缺失 → mismatch exit 1；缺证据 + 版本不一致 → exit 1 | 通过 |

RV-02、RV-03 及原有用例全部保留并通过（夹具 28 → 37 项）。审计探针 `.planning/audit-0.8.10-20261115/probes.py` 对修复后代码重跑：

- baseline exit 0；
- missing_counts exit 3；
- operations_query_failure exit 3，说明为 “operations_since_start not collected: sqlite error (OperationalError)”；
- cancelled_inflight、unknown 均 exit 3；
- remote_first 为 match；
- error_redaction 的 `marker_in_output=false`。

## 2. 测试

| 命令 | 结果 |
| --- | --- |
| `python3 -m unittest discover -s scripts/release-verify` **[夹具]** | Ran 37，OK |
| `make check`（race 测试、vet、gofmt、staticcheck） | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.11` |
| `node --check`（页面与检查脚本）、`bash -n run.sh`、`py_compile`、`docker compose config -q`、`git diff --check` | 全部通过 |
| `scripts/ui-check/run.sh`（Chrome for Testing 156.0.8078.47）**[本地]** | exit 0；每个宽度（1280／375／390）settings 45、model 63、ownership 87、task-editor 68，页面错误 0 |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` **[本地]** | tests 27，pass 27，fail 0，skipped 0 |
| 审计探针 task-edge-check（LE-01／LE-02）、account-close-check（DP-01）**[本地]** | exit 0；三个宽度分别 12/12、6/6 |
| 运行后资源 | 无 `mail-auto-ui-check.*`、`gate-fixture-*` 或实例进程 |

**真实 CI [CI]**

| 运行 | 提交 | 结果 |
| --- | --- | --- |
| 分支 [43719732816](https://github.com/allen0039/mail_auto/actions/runs/43719732816) | `6cf2ebf` | check、ui 成功；夹具 Ran 37；门禁 pass 27、skipped 0；遗留 0 |
| main [43720332863](https://github.com/allen0039/mail_auto/actions/runs/43720332863) | `6cf2ebf` | check=success，ui=success |
| tag v0.8.11 [43720334436](https://github.com/allen0039/mail_auto/actions/runs/43720334436) | `6cf2ebf` | check=success，ui=success |

## 3. 发布与镜像

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `6cf2ebff9d65ddc5e06390e65518244235e7a658`，main 由 `8b13855` 快进 |
| 标签 | `v0.8.11`（附注）→ `6cf2ebf` |
| index（0.8.11 与 latest 相同） | `sha256:4c3e7c77a272d148a454b817d73eb3f9932ed9dcd7131a7154afe5673e4278a5` |
| linux/amd64 | manifest `sha256:98d5cc2865f82865ff37f80332e87d17786b3036c2232bf37f56abb74188db9d`，config `sha256:ca21b33d3b202bd1770ed700951abea5c0a1bd4b29369f98b39d1eee10acaa86` |
| linux/arm64 | manifest `sha256:2ef566ca51ae47b64344669a381c19f43d3d95d64745d32f3f75fbb8536f6490`，config `sha256:c1c592b610f695f7031b21d43ca9b6542029b8aeb14922ae784bd97af492a215` |
| index 中另两项 | `unknown/unknown` 构建证明 |

## 4. Oracle1 **[生产只读]**

| 项目 | 结果 |
| --- | --- |
| 部署前 | 13:49:18Z：进行中运行 0；未完成操作只有 2026-11-12 起的 1 项既有待核验（未改动）；下一计划运行 14:00Z |
| 启动 | 2026-11-15T13:49:35Z |
| ImageID | `sha256:c1c592b610f6…a215`，等于 arm64 config；RepoDigest 为上方 index |
| 版本 | healthz ok 0.8.11；readyz ready、0.8.11、schema 13 |
| 健康／重启 | running、healthy、重启 0 |
| 文件夹同步 | 14/14 在 13:49:51–13:50:11Z 完成 |
| 页面资源 | 7 个文件的 SHA-256 与源码相同 |
| 日志 | 启动后 error／panic 0 条 |
| 其他服务 | 23 个容器的重启次数与启动时间与部署前相同 |
| 自然任务 | 采集时（13:50Z）部署后没有运行，记为未核对；未等待、未触发。本版没有真实写入或模型调用 |

## 5. 新入口采集结果与一致性 **[生产只读]**

命令（`--remote origin` 放在末位）：

```text
release_verify.py --version 0.8.11 --commit 6cf2ebf --ssh <Oracle1> --compose-dir <Compose 目录> --backup <…/upgrade-0.8.11-20261115-134918> --remote origin
```

- 13:49:59–13:50:29Z 连续运行两次：都 exit 3，去掉时间后 JSON 完全相同。
- 12 项 match：标签、main（即发布提交）、latest 等于版本、双架构、服务器镜像、版本、健康、重启 0、文件夹 14/14、配置表 16/16（schema 13→13）、任务用户字段 4/4、主密钥一致（只输出布尔值）。
- 1 项未核对：部署后任务 “no runs since start”。
- 部署文件：`compose.yaml`、`1panel.env` 与本次升级前备份逐字节相同，也与 0.8.10 升级前备份相同（单独 `cmp`；入口不比较部署文件）。
- 任务运行字段在采集时都未推进；部署后新增操作 0；既有待核验操作 1，未改动。

只读证明：两次采集前后对 15 张表做行数与内容摘要的独立只读快照，有两处变化，均来自应用自身：

- `mailboxes`：首次同步在 13:49:51–13:50:11Z 进行，与快照时间重叠；
- `task_runs`：2026-11-12 的 needs_verification 手动运行 `WfT5wXFL…` 在 13:50:35Z 被应用对账更新了 `updated_at`，状态不变。0.8.9、0.8.10 部署后同样出现过。

两处之外：审计 0 条，部署后操作 0 条。入口连接为 `mode=ro` + `query_only`。

## 6. 备份、回滚与清理

- 备份 `backups/upgrade-0.8.11-20261115-134918`（目录 700，数据库与主密钥 600）：一致性快照（integrity ok、schema 13）、主密钥（与在线密钥逐字节相同）、compose 与环境文件、其他服务清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.10`（保留）；未做回滚演练。发布记录 `releases/0.8.11.json`。
- 已删除：
  - `backups/upgrade-0.8.10-20261115-123159`：删除前确认在线主密钥、部署文件与其相同；
  - 镜像 `allen0039/mail-auto:0.8.9`：无容器使用。
- 保留：0.8.11／latest、回滚镜像 0.8.10、全部发布记录、部署配置、邮件数据与密钥。cloudflared 的悬空镜像不属于本项目，未处理。
- 服务器上没有留下临时脚本（全部经 SSH 标准输入执行）。

## 7. 未核对与已知限制

- 0.8.11 部署后尚无自然任务运行（下一计划 14:00Z），记为未核对。
- 部署前独立复验时已有一次 0.8.10 下的自然 noop；noop 不证明真实分类写入。
- “已收尾”依赖服务写入运行摘要的字段；入口不读逐项操作明细，也不比较部署文件、不检查 CI、不替代独立审计。
- 核对期间 SSH 偶尔断开，均在重试后成功；SSH 失败时入口把服务器项标为未核对。

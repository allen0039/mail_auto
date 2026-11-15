# 0.8.12 验收记录：只读发布核对入口 RV-05 修复

范围：只修 0.8.11 独立复验的 RV-05（P2，`scripts/release-verify/release_verify.py` 的失败优先级）。

- 产品代码、界面、CI 工作流、UI 检查与门禁不变；数据库仍为 schema 13，无迁移；没有新增依赖。
- 不实施原方案第三项。
- 本文件不声明验收通过，交由 Codex 独立复验。

证据标记：**[夹具]** 临时 SQLite、假外部命令或真实子进程；**[本地]** 本机隔离实例；**[CI]** GitHub Actions；**[生产只读]** Oracle1 只读查询。

## 1. RV-05

### 根因

0.8.11 的 `classify_run` 判断顺序是：

1. 进行中状态；
2. failed、partial、interrupted 状态；
3. needs_verification 状态；
4. 必需字段是否完整；
5. failed 计数。

结果有两种问题：

- 同一运行既有 `failed>0` 又是 needs_verification 状态时，在第 3 步就返回待核验。服务正是这样写这种运行的：`task_runner.go` 先按 `Failed>0` 记失败，再因 `Unknown>0` 改为 needs_verification。
- `failed>0` 但其他字段缺失或格式错误时，在第 4 步就返回证据缺失。

两种情况下明确的失败都被遮盖，报告只给出未核对、exit 3。

### 修改（`classify_run`）

- 先把计数字段读出来：载荷是 JSON 对象时，用 `count` 只取格式有效的非负整数，缺失或格式错误为 None，不当 0。
- 第一步判断明确失败：状态为 failed、partial、interrupted，或格式有效的 failed 大于 0 → failed，对应不一致、exit 1。进行中、待核验、缺字段等都不能覆盖它。
- 没有明确失败时，后续判断与 0.8.11 相同：
  - 载荷不是对象 → missing_evidence；
  - 进行中 → running；needs_verification → 待核验；
  - 必需字段缺失或格式错误 → missing_evidence；
  - `unknown>0` → 待核验；
  - 未收尾或仍有 queued、running → not_finalized；
  - 已收尾时：noop、completed、observed 通过；cancelled、limited、requires_ai、未知状态为未核对。
- 格式错误的 failed（字符串、负数、浮点、布尔、数组、缺失）既不算失败，也不当 0。
- RV-04 的分段采集、操作查询失败为未核对、正常空结果的处理都没有改。

### 回归（`FailurePrecedenceTest`，8 项）**[夹具]**

每个用例都经过完整入口，核对 JSON 中的 `result`、`exit_code`、`mismatched`／`unverified`、检查项状态与说明、标准错误中的逐项结论行与总结行，以及进程返回值。

| 用例 | 结果 |
| --- | --- |
| 审计复现：完整载荷 state=needs_verification、failed=2、unknown=1、queued=0、running=0、finalized=true | mismatch、exit 1，只有任务项不一致；证据中该运行 class=failed、failed=2、unknown=1；终端为 “mismatch … failed 1” 和 “release-verify 0.8.9: mismatch (exit 1)” |
| failed=2 同时其他字段缺失或格式错误（finalized、unknown、queued、running 缺失；unknown="1"；queued=-1；finalized="yes"；未收尾且 running=1）× 6 种状态（cancelled、completed、noop、needs_verification、running、未知），另加只有 `{"failed": 2}` 的 cancelled | 全部 mismatch、exit 1 |
| failed、partial、interrupted 状态，载荷无计数、不是 JSON，或 failed="x" | 全部 mismatch、exit 1 |
| 同时有待核验运行、只含 finalized 的运行、failed=1 的 completed 运行，且操作表缺失 | mismatch、exit 1（失败优先于其他未核对） |
| failed 为 "2"、-1、2.0、true、[2] 或缺失（其余字段有效的 noop） | unverified、exit 3、missing_evidence；既不判失败也不当 0 |
| 没有明确失败：needs_verification（完整或只有 finalized）、noop 加 unknown=1、只有 finalized、completed 未收尾、未知状态、进行中、部署后操作查询失败 | 全部 unverified、exit 3，“unverified … (exit 3)” |
| 正常 noop、observed、completed（有效零值） | match、exit 0；证据中 failed、unknown、queued、running 均为 0 |
| RV-04 两条原始反例：只有 `{"finalized": true}` 的 noop；已采到 running 操作但部署后操作查询失败 | unverified、exit 3；open_operations 采到 running=1，operations_since_start 为 null |

夹具合计 37 → 45 项，全部通过。原有用例都没有修改，其中包括 RV-02 的错误脱敏和 RV-03 的参数顺序。

审计探针重跑结果：

- `.planning/audit-0.8.11-20261115/probes.py`：
  - 待核验且有失败：exit 1；
  - cancelled 或 completed 有失败且缺字段：exit 1；
  - 有失败且缺 finalized：exit 1；
  - 有失败、字段完整：exit 1；
  - 有 unknown 但缺 failed：exit 3。
- `.planning/audit-0.8.10-20261115/probes.py`：
  - 正常情况 exit 0；
  - 缺计数、操作查询失败、未知状态、cancelled 在途均为 exit 3；
  - `--remote` 放首位为 match；
  - 错误脱敏 `marker_in_output=false`。

## 2. 测试

| 命令 | 结果 |
| --- | --- |
| `python3 -m unittest discover -s scripts/release-verify` **[夹具]** | Ran 45，OK |
| `make check`（race 测试、vet、gofmt、staticcheck） | exit 0 |
| `CGO_ENABLED=0 go build -trimpath ./cmd/mail-auto` + `version` | exit 0，`0.8.12` |
| `node --check`（页面与检查脚本）、`bash -n run.sh`、`py_compile`、`docker compose config -q`、`git diff --check` | 全部通过 |
| `scripts/ui-check/run.sh`（Chrome for Testing 156.0.8078.47）**[本地]** | exit 0；每个宽度（1280／375／390）settings 45、model 63、ownership 87、task-editor 68，页面错误 0 |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` **[本地]** | tests 27，pass 27，fail 0，skipped 0 |
| 审计探针 task-edge-check（LE-01／LE-02）、account-close-check（DP-01）**[本地]** | exit 0；三个宽度分别 12/12、6/6 |
| 运行后资源 | 无 `mail-auto-ui-check.*`、`gate-fixture-*` 或实例进程 |

**真实 CI [CI]**

| 运行 | 提交 | 结果 |
| --- | --- | --- |
| 分支 [43726365685](https://github.com/allen0039/mail_auto/actions/runs/43726365685) | `387d50d` | check、ui 成功；夹具 Ran 45；门禁 pass 27、skipped 0；遗留 0 |
| main [43726979181](https://github.com/allen0039/mail_auto/actions/runs/43726979181) | `387d50d` | check=success，ui=success |
| tag v0.8.12 [43726982399](https://github.com/allen0039/mail_auto/actions/runs/43726982399) | `387d50d` | check=success，ui=success |

## 3. 发布与镜像

| 项目 | 结果 |
| --- | --- |
| 发布提交 | `387d50d1ea0c93a80be6f813be2b231b3d9c5b8a`，main 由 `66bb8b9` 快进 |
| 标签 | `v0.8.12`（附注）→ `387d50d` |
| index（0.8.12 与 latest 相同） | `sha256:f3b86bdbb32c6d9c114e56536c77481340b4a82a5dcbe78551b7cd5ebf752ac6` |
| linux/amd64 | manifest `sha256:a27df41eb7e148633add9c3776b87932a15a0a2a770afa9dfc73dbeffefb03f5`，config `sha256:21b4755bfea686a0be6bc0d12e834e15194e4b37d693eedb2779e2c0acfcbab9` |
| linux/arm64 | manifest `sha256:0fa7084d1dc34fb4c46b608b059afa05d111399f2824b80a82d6b964fa73ab2f`，config `sha256:8de0eb4f672a9223485d14b323780006b9b0ecdd9f5cd0e3ff10bcd3b84e182c` |
| index 中另两项 | `unknown/unknown` 构建证明 |

## 4. Oracle1 **[生产只读]**

| 项目 | 结果 |
| --- | --- |
| 部署前 | 15:03:21Z：进行中运行 0；未完成操作只有 2026-11-12 起的 1 项既有待核验（未改动）；下一计划运行 16:00Z |
| 启动 | 2026-11-15T15:03:36Z |
| ImageID | `sha256:8de0eb4f672a…e182c`，等于 arm64 config；RepoDigest 为上方 index |
| 版本 | healthz ok 0.8.12；readyz ready、0.8.12、schema 13 |
| 健康／重启 | running、healthy、重启 0 |
| 文件夹同步 | 14/14 在启动后完成，最近 15:06:14Z |
| 页面资源 | 7 个文件的 SHA-256 与源码相同 |
| 日志 | 启动后 error／panic 0 条 |
| 其他服务 | 23 个容器的重启次数与启动时间与部署前相同 |
| 自然任务 | 0.8.12 采集时（15:07Z）部署后没有运行，记为未核对；未等待、未触发。0.8.11 运行期间有两次内置自然运行（14:00Z、15:00Z），均为 noop，不证明真实写入。本版没有真实写入或模型调用 |

## 5. 新入口采集结果与一致性 **[生产只读]**

命令（`--remote origin` 放在首位）：

```text
release_verify.py --remote origin --version 0.8.12 --commit 387d50d --ssh <Oracle1> --compose-dir <Compose 目录> --backup <…/upgrade-0.8.12-20261115-150321>
```

- 15:06:36–15:07:06Z 连续运行两次：都 exit 3，去掉时间后 JSON 完全相同。
- 12 项 match：标签、main（即发布提交）、latest 等于版本、双架构、服务器镜像、版本、健康、重启 0、文件夹 14/14、配置表 16/16（schema 13→13）、任务用户字段 4/4、主密钥一致（只输出布尔值）。
- 1 项未核对：部署后任务 “no runs since start”。
- 部署文件：`compose.yaml`、`1panel.env` 与本次升级前备份逐字节相同，也与 0.8.11 升级前备份相同（单独 `cmp`；入口不比较部署文件）。
- 采集时任务运行字段都未推进；部署后操作 `{}`；既有待核验操作 1，未改动。

只读证明：两次采集前后对 15 张表做行数与内容摘要的独立只读快照，只有 `task_runs` 一行不同。

- 那是 2026-11-12 的 needs_verification 手动运行 `WfT5wXFL…`，应用在 15:07:12Z 自行对账更新了 `updated_at`，状态不变。0.8.9～0.8.11 部署后同样出现过。
- 这期间审计 0 条，部署后操作 0 条。入口连接为 `mode=ro` + `query_only`。

## 6. 备份、回滚与清理

- 备份 `backups/upgrade-0.8.12-20261115-150321`（目录 700，数据库与主密钥 600）：一致性快照（integrity ok、schema 13）、主密钥（与在线密钥逐字节相同）、compose 与环境文件、其他服务清单。
- 回滚：同 schema，可直接改回 `allen0039/mail-auto:0.8.11`（保留）；未做回滚演练。发布记录 `releases/0.8.12.json`。
- 已删除：
  - `backups/upgrade-0.8.11-20261115-134918`：删除前确认在线主密钥、部署文件与其相同；
  - 镜像 `allen0039/mail-auto:0.8.10`：无容器使用。
- 保留：0.8.12／latest、回滚镜像 0.8.11、全部发布记录、部署配置、邮件数据与密钥。cloudflared 的悬空镜像不属于本项目，未处理。
- 服务器上没有留下临时脚本（全部经 SSH 标准输入执行）。

## 7. 未核对与已知限制

- 0.8.12 部署后尚无自然任务运行（下一计划 16:00Z），记为未核对。
- 生产中没有出现过“失败加待核验”的组合，RV-05 的失败分支只由夹具与审计探针验证。
- “已收尾”和失败判断依赖服务写入运行摘要的状态与计数；入口不读逐项操作明细，也不比较部署文件、不检查 CI、不替代独立审计。
- 核对期间 SSH 偶尔断开，均在重试后成功；SSH 失败时入口把服务器项标为未核对。

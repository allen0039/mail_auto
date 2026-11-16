# Gmail / Outlook 实施交接

交接日期：2026-11-16。最新开发目标：0.10.2 / schema 15。第 13–14 节保留应用复用、目录诊断的历史交接；本轮 Gmail 安全修复、最终门禁和固定候选交接见第 15 节。

**等待主代理独立验收；尚未发布部署。** 第 1–11 节保留初次实施及其失败前的自测记录，第 12 节保留 GO-IA-01～04 返工交接。返工候选 `574b047` 已经独立代码复验通过，原报告和失败证据均保持原样；该结论不覆盖新增 0.10.1/0.10.2 代码。旧章节的“当前”“未准备”等状态均为当时记录，以第 15 节及真实验收文档最新记录为准。模拟测试完成，真实账号整体验收待完成；当前不满足生产发布验收条件。

最新本地候选为 **0.10.2/schema15**。本轮仅在本机隔离 QA 验证，未推送、发布或部署；新增代码与真实证据仍须主代理独立复验。

## 1. 基线与候选

| 项目 | 实际值 |
| --- | --- |
| 源码目录 | `/Users/allen/Downloads/Agent_Worker/mail_auto` |
| 基线 | main `15c7160827d1ae56d3e177f818c02c450c53697e`，0.9.2 / schema 14 |
| 固定代码候选 | `207159cddd9706c3c12e0817e34402faab59891d`（本地提交，64个文件；未推送） |
| 目标 | 0.10.0 / schema 15，未覆盖其他迁移或标签 |
| 标签检查 | 本地及 `git ls-remote --tags origin 'refs/tags/*0.10*'` 未发现占用，命令退出 0 |
| 发布状态 | 未打标签，未推送镜像或 latest，未部署 Oracle1，未清理生产资源 |
| 仓库工作流 | 仅 `.github/workflows/ci.yml`，push / PR 执行检查及 `push: false` 双架构构建，无自动生产发布 |

基线与方案编制时一致。开始时没有其他人的已跟踪源码差异；原有未跟踪 `AGENTS.md`、`docs/gmail-outlook-implementation-plan.md`、`docs/gmail-outlook-collaborator-prompt.md` 和既有 `.planning/` 内容均保留，未纳入候选。候选固定后工作区状态及交接文档提交见本节末尾的最终记录。

候选 tree 为 `eafdcd4a331e4bdd7223fc0318f4affc6f87089e`。本报告以候选后的独立文档提交保存，该提交仅增加本报告，不改源码和已测产物。交接后已跟踪文件无未提交差异；保留的未跟踪项为 `.planning/`、`AGENTS.md`、`docs/gmail-outlook-implementation-plan.md`、`docs/gmail-outlook-collaborator-prompt.md`。64个候选文件逐一列于本地 `validation/candidate-precommit-scan.json`，提交前路径/敏感模式扫描退出0、命中0，`git diff --cached --check`退出0。

## 2. 实施结果及变更文件

| 文件或文件组 | 实现说明 |
| --- | --- |
| `internal/mailoauth/{model,http,identity,manager}.go` | 固定官方端点、scope、中文安全错误、PKCE S256、十分钟一次性 state、会话/账号/配置/网络绑定、OIDC RS256/JWKS/issuer/audience/expiry/nonce、稳定 sub/oid+tenant 身份、Vault 加密应用密钥和 refresh token、并发刷新合并及 CAS |
| `internal/store/mail_oauth.go`、`store.go` | schema 14→15 单事务迁移；OAuth 应用、账号、请求、Gmail 目录角色和运行领取表；messages / sync_stage 的字符串 Gmail ID 及索引；账号删除外键级联 |
| `internal/admin/mail_oauth.go`、`server.go` | 应用配置 API、受管理员/非受限会话/CSRF 保护的授权和本地断开；独立于 MCP 的回调；先验证身份并只读 IMAP/LIST，再绑定；303 去掉 code/state；no-store / no-referrer |
| `internal/imap/{oauth,client,errors}.go` | 类型明确的 credential source 与 SASL XOAUTH2；OAuth 不传入 password；每次实际重连重新取有效 token；连接代际/路由校验，SASL/provider 文本不回显 |
| `internal/imap/{gmail,write,flags,manual_resolution}.go` | X-GM-EXT-1、精确字符串 X-GM-MSGID、SPECIAL-USE；普通标签添加目标/移除指定源、INBOX 归档；稳定 ID 只读核验；拒绝 Gmail COPY/MOVE/Deleted/EXPUNGE、Trash/恢复及两份副本结案；STORE 未知结果不盲重放 |
| `internal/mail/{oauth,network,service,model}.go` | 新服务商白名单、待授权账号、账号状态和唯一/条目数、外部 secret 兼容、显式账号 SQL 列、配置变化关闭连接/作废缓存与旧 worker、独立路由诊断 |
| `internal/mail/{automatic,incremental,sync,metadata}.go` | 后台/手动同步链路、Gmail ID 持久化、OAuth 待授权不启动 worker、旧快照发布防护 |
| `internal/mail/{gmail,automation,automation_policy,rules,task_readiness}.go` | 同逻辑邮件去重、并发排他、运行范围领取、确定多源规则次序、跨成员保护；能力/规则/任务就绪提示 |
| `internal/mail/{operations,operations_recovery,recovery,flags,flags_recovery,manual_resolution}.go` | 预览/执行/恢复共用服务端限制；人工保留保护跨 Gmail 视图；标记跨视图更新；未知标记只读核验；未知标签写入只核验、不补发 |
| `internal/admin/web/{app.js,index.html,organizer.js,tasks.js}` | 应用设置与精确 HTTPS 回调、服务商预设、官方授权/重授权/本地断开、中文状态、Gmail 限制和统计口径；表单草稿、取消切换、关闭重开及迟到响应/迟到跳转保护 |
| `internal/mailoauth/oauth_test.go`、`internal/admin/mail_oauth_test.go`、`internal/store/mail_oauth_test.go` | OAuth、会话权限、身份、刷新竞争、网络/泄漏、原子迁移及回滚测试 |
| `internal/imap/{gmail_test,oauth_dial_test,flags_test}.go`、`internal/mail/{gmail_test,flags_recovery_test}.go` | 实际 TLS/CONNECT/XOAUTH2 本地集成、重连重新取 token、Gmail 标签协议、能力拒绝、多源规则/保护、未知结果及缺失 FLAGS 不得判成功测试 |
| `internal/mail/{manual_resolution_test,run_counts_test}.go`、`internal/store/manual_resolution_test.go` | 既有测试的显式消息列增加 Gmail ID，保留原有行为断言 |
| `scripts/ui-check/mail-oauth-ui-check.js`、`run.sh`、`.github/workflows/ci.yml` | OAuth UI 检查进入默认六脚本 × 三宽度门禁，保留原 gate 的 27 条断言 |
| `scripts/release-verify/{release_verify,test_release_verify}.py` | 0.10.0 / schema 15 发布核对映射和夹具 |
| `scripts/gmail-outlook-verify.py`、`docs/gmail-outlook-test-results.json`、`docs/gmail-outlook-image-build.json` | 可重复本地门禁、实际工具版本/退出码/数量/证据位置及本地 OCI 结果 |
| `internal/version/version.go`、`compose.yaml`、`README.md`、`CHANGELOG.md`、`docs/configuration.md`、`docs/gmail-outlook-setup.md` | 版本一致性、接入范围、注册、网络、升级/回退和真实验收步骤 |

运行时、UI、MCP 的版本均为 0.10.0；MCP 继续使用公共版本来源。Compose 的 0.10.0 标签是待发布候选配置，当前 registry 并没有因此产生该标签。没有改变原有任务、每批数量、未读/星标保护、代理或每日处理规则，也没有新增每日额度。

## 3. 全部认证与写入入口核对

| 入口 | 认证和安全链路 |
| --- | --- |
| 连接诊断、文件夹探测、正文、扫描 | Adapter.open → 账号路由 → dialRoute → AuthSource.Credential → XOAUTH2；旧服务商保留 LOGIN |
| 独立网络诊断 | Service.DiagnoseNetwork → diagnosticCredential.CredentialForRoute → Adapter.DiagnoseRoute → dialRoute；换 token 和 IMAP 都使用明确选择的诊断路由 |
| 后台/手动首次、增量同步、IDLE/轮询和重连 | 共用 open/dialRoute；重连重新取 token，代际/网络变化阻止旧连接及结果发布 |
| 规则任务、MCP 与 Agent 触发 | 复用 Service 规则/计划/操作接口；没有绕过服务端能力和保护检查的新增入口 |
| 移动/归档/Trash/标记 | executeItem → 能力/权限/保护复查 → WritableRemote / AutomationMoveRemote / SetFlags → open 或受代际约束的池连接 |
| 恢复、撤销、未知核验、清理重连 | 同一认证层；Gmail 稳定 ID 核验无 STORE；不支持的恢复和“保留两份”在服务端拒绝 |

源码搜索中，生产 IMAP 仅 `client.go` 的 dialRoute 包含 LOGIN / Authenticate；其余同名出现属于测试夹具。原有账号表不扩列；生产 `SELECT * FROM accounts` 和无列名创建 INSERT 已移除。旧迁移夹具中的裸 INSERT 保留，用于验证历史布局，没有作为新运行时代码继续使用。

## 4. 能力支持矩阵

“模拟通过”仅指本地自测；所有线上服务商行为仍须真实账号验收。

| 能力 | 个人 Gmail | 个人 Outlook / Hotmail / Live | Google Workspace | Microsoft 365 |
| --- | --- | --- | --- | --- |
| OAuth + PKCE + OIDC、自动刷新/轮换 | 实现，模拟通过，真实待验 | 实现，模拟通过，真实待验 | 兼容扩展，未验证 | common / 委托权限扩展，未验证 |
| IMAPS + XOAUTH2 / 目录 / 元数据 / 正文 | 实现，模拟通过，真实待验 | 实现，模拟通过，真实待验 | 未验证 | 未验证，受租户/IMAP 策略限制 |
| 分类移动 | 添加普通目标标签、移除指定源，保留其他标签；模拟通过 | 原有 MOVE / UIDPLUS 定向兼容路径；模拟回归通过 | 相同受限路径，未验证 | 原有受控路径，未验证 |
| 归档 | 仅 INBOX → 探测到的 All Mail，移除 Inbox 成员；模拟通过 | SPECIAL-USE / 管理员映射；模拟通过 | 未验证 | 未验证 |
| Trash / 从 Trash 恢复 | 未验证，服务端拒绝 | 原有受控路径，真实待验 | 同 Gmail，拒绝 | 未验证 |
| 标记已读、未读、星标 | 仅 Seen / Flagged，跨已知视图传播；模拟通过 | 原有路径，模拟通过 | 未验证 | 未验证 |
| 未知写入核验 | 稳定 ID 只读核验；不重放，不自动补齐未完成标签修改 | 原有受控移动核验；未知 STORE 仅回读 | 未验证 | 未验证 |
| 人工保留两份 | 拒绝，多个标签视图不证明独立副本 | 保留既有全文核验和持久保护 | 拒绝 | 未验证 |
| 移动/归档撤销（返工后的限制） | 拒绝：缺少操作前完整成员证据，反向 move 会有移除原有标签风险 | 保留原有受控反向计划 | 同 Gmail，拒绝 | 未验证 |
| 发信 / 永久删除 / 全局 EXPUNGE | 不提供 | 不提供 | 不提供 | 不提供 |

共享邮箱、代理访问、服务账号、应用级全租户访问、主权云、本地 Exchange 均不支持。Microsoft Graph token 不接受为 IMAP 权限。自定义邮箱域名须手选 Google / Microsoft，不按域名生成主机。

## 5. Gmail 身份、冲突与保护

- 账号 + X-GM-MSGID 是逻辑身份，ID 在 SQLite/Go/JSON 中均保持十进制字符串；文件夹 UIDVALIDITY / UID 继续定位协议操作。RFC Message-ID 不是 Gmail 逻辑 ID。
- 同一计划按逻辑身份去重；排队/执行/待核验操作及 AI 领取检查别名冲突。自动任务按 run（无 run 时按 batch）持久领取一次；后续新 run 可以有意再次处理。未知结果保留排他状态。
- 规则按 priority 升序、ID 升序；再按 folder/ref 固定次序选择源。首个匹配、未知或保护结果结束决策，矛盾动作不合并。人工保留、未读、星标和未知标记检查覆盖已知成员，实际自动写入前再读取远端。
- 分类为定向 X-GM-LABELS：加目标，再移除指定源；目标已存在可接受，无关标签保留。中途失败保留 unknown；不伪造 COPYUID。原子性无法保证的中间状态交由只读核验和人工检查，不自动重放。
- 目录角色来自 SPECIAL-USE 并保留原名。All Mail、Trash、Sent、Drafts、Starred、Important 及未知特殊属性不能伪装普通标签。角色探测结果受配置快照约束，迟到结果不覆盖新上下文。
- UI 总数/同步进度为文件夹条目，Gmail 卡片另列唯一邮件数。多个标签视图不显示为保留两份。

## 6. 与方案的偏差及影响

| 偏差或补充 | 原因、证据与影响 |
| --- | --- |
| Gmail Trash / 恢复 / 特殊目录移动未开放 | 没有真实账号证明 Gmail Deleted、Expunge 与特殊标签的安全语义。按方案 3.2/7 节允许的安全边界，在能力、规则保存、预览、执行、任务就绪、恢复和 UI 中拒绝。属于可见范围限制，不能算对应真实操作已验证；是否补齐由主代理/用户决定。 |
| Gmail 不走现有 COPY/MOVE/定向 EXPUNGE | 采用 X-GM-LABELS 表达“加目标、移指定源”，避免标签视图被当物理副本。协议夹具验证无 COPY/MOVE/Deleted/EXPUNGE，线上行为待验。 |
| Google 与 Microsoft 初次诊断失败处理不同 | Google 经验证 email 可绑定已验证主体，网络失败保存 network_error 以便重连。Microsoft 地址声明可能缺失，先实际 IMAP 只读诊断成功才绑定，避免用不可靠地址声明替代身份。 |
| Microsoft 仅 consumers/common authority | 限定个人优先及未验证组织扩展；不接受任意 tenant URL 或主权云。稳定身份用 oid+tid（若有），否则 sub+tid。首次地址声明存在但与填写地址不同会拒绝；别名/组织 UPN 差异需要以本人地址核对，不静默换邮箱。 |
| 没有供应商侧撤销 API | 本地断开立即停止工作并清除 token/state/cache；UI 明确仅本地断开，供应商侧撤销由用户在官方账号权限页完成。 |
| 没有真实账号证据 | 未提供 OAuth 应用、固定 HTTPS origin、官方网页登录和专用邮件，所有真实链路阻塞；没有用模拟结果代替，也没有操作私人邮件。 |
| 全目录格式扫描包含既有工作文件 | 实际命令退出 0，但列出三处既有 `.planning/` / `.claude/worktrees/` Go 文件，格式断言为非绿；未改动他人文件。候选源码格式和干净候选导出单独核验，见测试记录。 |

影响产品范围的 Gmail 限制和真实授权阻塞均保留为待决/待验事项，没有静默删除要求。

## 7. 自测命令、工具与证据

最终完整门禁在 11:45–11:52（+08:00）执行；最后一次全包 race 在 11:50:08 启动，11:51:52 完成，覆盖最终全部源码与测试。没有以 targeted 结果替代全包 race。

| 命令 | 退出码 | 通过 / 失败 / 跳过或检查结论 | 脱敏证据（validation/ 下） |
| --- | --- | --- | --- |
| `go test -race -json ./...` | 0 | 527 / 0 / 0 叶子测试；582个含父级的事件 | `final-race.jsonl`、`final-race-result.json` |
| `go vet ./...` | 0 | 静态命令，无测试计数；0诊断 | `vet-20261116-114938.log` |
| `gofmt -l .` | 0 | 列出3个既有工作文件；该格式断言非绿，见下文 | `format-20261116-114939.log` |
| `gofmt -l cmd internal scripts` | 0 | 未格式化源码0 | `source-format-20261116-114940.log` |
| `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...` | 0 | 静态命令，无测试计数；0诊断 | `staticcheck-20261116-114940.log` |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 0 | 可达符号漏洞0；非单测，依赖级附加结果见下文 | `govulncheck-20261116-114943.log` |
| `python3 -m unittest discover -s scripts/release-verify -v` | 0 | 46 / 0 / 0 | `release-fixtures-20261116-114949.log` |
| `docker compose config --quiet` | 0 | 配置检查，无测试计数 | `compose-20261116-114951.log` |
| `make build` | 0 | CGO_ENABLED=0本地构建成功；`bin/mail-auto version`为0.10.0 | `build-20261116-114951.log` |
| `scripts/ui-check/run.sh` | 0 | 1014 / 0 / 0断言，18脚本/宽度组合，页面错误0；OAuth为31×3=93断言 | `ui-20261116-114953.log` |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` | 0 | 27 / 0 / 0 | `ui-gate-20261116-115055.log` |
| `docker buildx build --builder mail-auto-oauth-validation --platform linux/amd64,linux/arm64 --output type=oci,dest=.planning/gmail-outlook-implementation/candidate-image.tar .` | 0 | 两架构完成，无push；另核对32个blob摘要和两个平台的manifest/config | `multiarch-20261116-115157.log` |
| `git diff --check` | 0 | 无空白差异错误；提交前另核对暂存内容 | `diff-20261116-115218.log` |

入口命令为 `MAIL_AUTO_TEST_BUILDER=mail-auto-oauth-validation python3 scripts/gmail-outlook-verify.py`。其实际退出1，唯一非绿断言是全目录格式扫描：`.claude/worktrees/acceptance-report-optimization-410bae/.planning/acceptance-round3-20261112/crash_flags_probe_test.go`、`.planning/acceptance-round3-20261112/crash_flags_probe_test.go`、`.planning/manual-resolution-092/preview-probe/main.go`。它们是本次之前的工作/诊断文件，未修改、未提交。不能把gofmt进程退出0解释成格式门禁通过；候选源码及干净导出检查另列，不隐藏该差异。

工具版本：Go 1.27.4 darwin/arm64；staticcheck 2026.2.2 (0.8.2)；govulncheck 1.9.0、漏洞库更新时间2026-11-11 02:51:11 UTC；Node 24.22.3；Python 3.9.6；Docker 29.8.2；Buildx 0.37.2；Compose 5.5.0；Chrome 157.0.8103.51；puppeteer-core 25.16.0、@puppeteer/browsers 3.2.6；Git 2.39.5 Apple Git-154；curl 8.7.1。版本命令均退出0。UI是本地Chrome157实际执行，仓库CI仍固定Chrome156.0.8078.47/Node24.14.0；未声称已运行远端CI或该浏览器版本。

完整命令数组、起始时间（Asia/Shanghai）、耗时、退出码、数量和历次门禁结果在 `docs/gmail-outlook-test-results.json`。原始本地日志位于 `.planning/gmail-outlook-implementation/validation/`，不进入 Git/镜像；以下矩阵为脱敏索引。Go 数量按叶子测试统计，父测试事件另记，不能将二者相加。

额外验证：

| 命令/实验 | 实际结果 | 证据 |
| --- | --- | --- |
| `go test ./internal/store ./internal/imap ./internal/mail -run 'Test(Gmail\|MailOAuthMigration)' -count=1` | 退出 1；新增跨视图人工保护预览断言失败，未删除断言；在 NewPlan 加入持久保护检查后修复 | `validation/final-targeted.log` |
| `go test ./internal/store ./internal/imap ./internal/mail -run 'Test(Gmail\|MailOAuthMigration\|ManualKeep)' -count=1` | 退出 0，3 包通过；非 verbose 输出未给单测/skip 总数；最终 race 包含这些测试 | `validation/final-targeted-2.log` |
| `go test -race -v ./internal/imap -run 'TestOAuth(Dial\|Rejects)' -count=1` | 退出 0；2 通过 / 0 失败 / 0 跳过；真实本地 TLS 和 CONNECT 路径、两服务商 XOAUTH2、断线后取下一个 token、独立诊断及错误机制拒绝 | `validation/oauth-tls-dial.log` |
| `PUPPETEER=puppeteer-core CHROME_PATH=… scripts/ui-check/run.sh scripts/ui-check/mail-oauth-ui-check.js` | 退出 0；29 × 3 = 87 断言通过 / 0 失败 / 0 跳过，页面错误 0 | `validation/oauth-ui-final.log` |
| `go test -race -json ./internal/imap ./internal/mail ./internal/mailoauth -run 'Test(FlagConfirmationRequiresObservedFlags\|UnknownFlagWriteIsOnlyReadBack\|WrongPKCEVerifierCannotAuthorize)$'` | 最初退出1，4叶子通过 / 1失败 / 0跳过，新增协议夹具缺raw socket而panic；补全夹具后退出0，6叶子通过 / 0失败 / 0跳过 | `validation/final-flags-pkce.jsonl`、`validation/final-flags-pkce-2.jsonl` |
| `go test -race -json ./internal/mailoauth -run TestAccessExpiryAndRestartRefresh -count=1` | 退出0；2叶子通过 / 0失败 / 0跳过（Google、Microsoft各1） | `validation/access-expiry-restart.jsonl` |
| 基线 `git archive 15c7160…`、`go build -trimpath … ./cmd/mail-auto`、旧二进制 `version` / `backup` | 构建退出0、版本输出0.9.2/退出0；schema15合成SQLite夹具的backup退出1且为预期“database schema is newer than this binary”；实验1通过 / 0失败 / 0跳过 | `validation/old-binary-schema15.json` |
| `git archive 207159cddd9706c3c12e0817e34402faab59891d` 后在导出目录执行 `gofmt -l .` 与空输出断言 | 两命令均退出0，未格式化0；实验1通过 / 0失败 / 0跳过；导出随后删除 | `validation/candidate-clean-format.json` |
| Python3读取本地OCI tar、逐个blob计算SHA-256、递归index并核对manifest/config平台 | 退出0；2平台通过 / 0失败 / 0跳过，32个blob摘要均匹配 | `docs/gmail-outlook-image-build.json` |

以上 `validation/…` 路径均相对于 `.planning/gmail-outlook-implementation/`。额外命令的完整 argv、工具输出和实际退出码保存在对应本地 JSON/日志，不包含真实服务商响应或凭据。

本地OCI归档的SHA-256为`9809360159c79c278861895414d7f12e0395e9a62f7c3b9cb01a0f54bf8d9820`；两个平台的manifest/config摘要见`docs/gmail-outlook-image-build.json`。这是工作区候选的本地构建产物和摘要，不是registry发布摘要或运行验证；没有创建或推送版本/latest标签。复跑时先创建独立builder：`docker buildx create --driver docker-container --name mail-auto-oauth-validation --bootstrap`，完成后仅移除该builder，不更改其他builder或服务。

开发中失败及修复保留：新增 Gmail ID 时消息 INSERT 的 22/23 列错误；OAuth 夹具多语句参数绑定错误；Gmail 协议夹具未进入 authenticated 状态；新增 Gmail 夹具继承额外消息及未知 flags；staticcheck 文案以大写开头；新增人工保护预览断言。均修复实现/夹具原因并保留行为断言，最终完整门禁重新执行。首轮 UI 的 Chrome `Target closed` 为浏览器运行失败，后续默认全量 UI 与 OAuth 单项重新通过；首轮 OCI exporter 的默认 Docker driver 不支持，改用独立 docker-container Buildx builder 后成功。早期非 JSON 开发测试没有可靠的逐叶数量，数量记为未采集，不冒充零失败；最终 JSON 门禁为候选证据。

govulncheck 的可达符号结果为 0；同时报告 2 个导入包漏洞、1 个依赖模块漏洞未被本代码调用。未更改 go.mod/go.sum 或以此声称所有依赖没有漏洞。

## 8. 方案第 10 节矩阵逐项对应

| 类别 | 已实际执行的本地证据 | 真实/外部限制 |
| --- | --- | --- |
| 迁移/兼容 | TestMailOAuthMigrationFromFourteen / RollsBack；新库、14升级、重复启动、失败重启；旧密文/secret文件名/任务 payload 原值；一致快照与旧二进制 guard | 未触碰生产库；生产备份/回退演练须隔离副本 |
| OAuth | StatePKCEAndBinding / StateRejectsInvalidContext；正常、拒绝、缺失/伪造/过期/重放、错 provider/session、受限会话、管理员轮换、配置/删除/新请求；Admin API 权限及 MCP 关闭回调 | 官方网页同意、实际回调 origin 待验 |
| 身份/scope | OIDCVerificationAndStableIdentity / OAuthEndToEndSimulated / ExchangePKCEIdentityAndScope / WrongPKCEVerifierCannotAuthorize；签名、issuer、audience、expiry、nonce、算法、主体、tenant、缺地址；缺邮件权限/Graph scope 拒绝；错误verifier的invalid_grant不得写入绑定；重授权换人拒绝 | Microsoft 别名、组织 UPN/tenant 实际声明待验；真实服务商PKCE挑战与scope返回仍需实际授权证明 |
| 刷新并发 | ConcurrentRefreshAndRotation / RefreshRaces / AccessExpiryAndRestartRefresh；20并发仅1请求，Google保留、Microsoft轮换，重授权/删除/断开/配置/写回失败竞争；临近过期及管理器重启均从加密refresh发起模拟refresh grant | 真实 refresh grant、服务商撤销与密钥到期待验；本地过期模拟不当作真实刷新证据 |
| 网络 | TokenFailuresAreSanitizedAndBounded / TokenNetworkPolicyAndExplicitDiagnosticRoute；network 包 DNS/CONNECT/TLS/超时/代理不直连；429/5xx/1MiB/取消/禁止redirect；TLS集成用受信测试CA且没有关闭证书验证 | 官方网络及当前代理出口真实可用性待验 |
| XOAUTH2 | XOAUTH2WireAndChallenge；两服务商精确报文、空challenge答复、断线、注入拒绝；OAuthDialAndDiagnosticRefreshOnReconnect；OAUTHBEARER机制拒绝 | 服务商 SASL、早于本地预期的 token 撤销/到期行为待验 |
| 同步 | 既有 AutomaticProtocolIncrementalSparseUIDs、IdlePushAndCancellation、IdleFallbackAndDisconnect、SyncResumesAfter700WithRealProtocolAndSQLite、UIDVALIDITY重连、正文PEEK、自动重连/删除/标记/文件夹回归；新增OAuth重连与稳定ID夹具 | 既有同步集成使用本地 IMAP；OAuth grant + 长历史/IDLE + 实际服务重启的整体服务商链路待验 |
| Gmail | LogicalPlansAndConcurrency、MultiSourceRuleOrderAndProtection、FolderRolesAndStaleProbe、ManualProtectionAcrossViewsWithoutRFCIdentity、StableIDAndReadOnlyReconciliation；64位ID、唯一/条目统计、目标已存在、无关标签与跨视图保护 | All Mail及多标签线上索引/并行任务待验 |
| 写入/恢复 | LabelMoveAndArchive：分类/归档/现有目标/保护改变/错ID/STORE断线/特殊源拒绝；RestrictedActionsAndFlags；UnknownWithoutMappingUsesStableIdentityEvidence；UnknownFlagWriteIsOnlyReadBack、FlagConfirmationRequiresObservedFlags明确区分空flags与未观察到flags；既有 MOVE/UIDPLUS/撤销/权限/恢复/人工保留回归 | Gmail Trash/恢复按拒绝验收；Outlook真实分类/归档/Trash/恢复/标记待验 |
| 旧功能 | OAuthAccountLifecycleAndLegacyCompatibility；全包 race 包括 QQ/网易/iCloud 输入/连接夹具、外部 secret、MCP OAuth、规则任务、Agent权限和人工保留 | 未连接旧生产邮箱；不得把模拟当成其线上回归 |
| UI | 默认六脚本、1280/375/390；OAuth 31断言/宽度；未配置引导、不回显、权限受限状态、取消切换、A→B→A、迟到load/save/start、关闭重开、草稿和中文拒绝提示；原27项gate全部保留 | 完整官方登录跨站重定向仍需真实HTTPS浏览器会话 |
| 泄漏 | API/app JSON隐藏密钥、SASL/provider错误脱敏、授权URL不带secret、回调去code/state、测试用合成身份；候选暂存内容/差异扫描 | 未接收真实secret/token/邮件；未来真实验收产物须再次脱敏 |

这张矩阵区分组件/本地集成和服务商整体链路；不能把几个本地组件测试相加描述成真实账号端到端通过。所有自动测试跳过数量为实际工具输出；真实账号阻塞另列，不藏在 skip 中。

## 9. 真实账号验收记录

| 账号代号 | 类型 | OAuth应用/官方登录/专用数据 | 实际写入 | run / operation ID | 结论 |
| --- | --- | --- | --- | --- | --- |
| G-PERSONAL-01 | 拟用个人 Gmail，尚未登录 | 全部未提供 | 0 | 无 | 真实账号验收待完成 |
| MS-PERSONAL-01 | 个人 Outlook；官方授权、IMAP诊断及重启后刷新路径通过；QA邮件未准备 | 应用已保存，未做专用邮件整理 | 0 | 无 | 等待专用 QA 数据 |
| Workspace / M365 | 无组织测试账号 | 未提供 | 0 | 无 | 未验证兼容扩展 |

Gmail 官方登录、IMAP、刷新、同步和整理均待完成。Outlook 已完成官方授权、IMAP诊断和重启后的真实 refresh 路径；专用目录同步、重授权恢复、专用邮件整理及规则任务仍待完成。两个账号均无真实写入或 run/operation ID；模拟测试生成的 ID 没有冒充真实证据。

最少用户操作：

1. 准备浏览器可达的固定 HTTPS origin 和私有测试实例；在 Google Cloud / Microsoft Entra 注册 Web OAuth 应用，在受保护的“邮箱授权应用”填写 client ID / secret / origin，不向聊天提供密钥。
2. 精确注册管理界面展示的两个回调；Google添加本人测试用户，Microsoft账号类型覆盖个人账号；本人在官方页面登录同意。
3. 用本人准备的 MailAuto-QA / MailAuto-QA-Target 专用文件夹/标签和明确选中的测试邮件；Gmail同一邮件挂两个测试标签并另留无关标签，另备未读、星标、人工保护反例。不随意操作私人邮件。
4. 在该范围执行方案第11节全部真实步骤。一次真实token endpoint refresh grant成功后重新连接，记录脱敏时间；只改本地过期时间不算通过。分类/归档/标记、Outlook Trash/恢复、受控规则记录真实run/operation ID，与网页核对；Gmail拒绝项记录服务端拒绝。
5. 将账号代号、权限、范围、前后状态、保护跳过、真实run/operation ID写回本报告，再交主代理独立验收/复验。真实证据补齐前不发布。

## 10. 应用注册、网络、升级与回退

完整操作说明见 `docs/gmail-outlook-setup.md`，关键参数如下：

- Google scope：`openid email https://mail.google.com/`；Microsoft：`openid email profile offline_access https://outlook.office.com/IMAP.AccessAsUser.All`。不申请额外 SMTP、联系人、日历、Graph 或应用权限。
- 回调精确为 `https://固定origin/api/mail-oauth/google/callback` 与 `/api/mail-oauth/microsoft/callback`。origin 来自管理配置，不能以 Host/X-Forwarded-Host 拼接；代理日志不记录query。会话回调辅助Cookie为Secure/HttpOnly/Lax/十分钟/窄path，管理员主Cookie仍为Strict。
- Google 外部应用 Testing 涉及邮件权限时refresh token通常七天到期；Production不等于已验证或永久有效。全邮件scope比产品操作范围更广，公开restricted scope应用须按官方政策确认验证/评估要求。
- Microsoft默认consumers；common为个人+组织未验证扩展。组织还受注册/同意权限、用户分配、条件访问和Exchange IMAP开关约束。相同client ID更新secret增加配置版本、保留refresh并重新检测；身份配置变化要求重新授权。密钥到期由管理员在UI记录并按官方期限轮换。
- 浏览器登录走浏览器网络；token/JWKS/IMAP走账号选定网络。固定Google accounts/oauth2/www.googleapis.com JWKS与微软login.microsoftonline.com端点；IMAPS为imap.gmail.com:993、outlook.office365.com:993。代理失败不直连；HTTP 25秒、1MiB上限、TLS验证、禁止redirect。

升级仅在独立验收及两个个人真实账号通过后进行：

1. 暂停远端整理，记录原任务/保护/批次/网络配置。用旧二进制 `backup` 一致快照，另备配套master.key、外部secret和私有部署配置，目录0700/文件0600；不只复制活动SQLite主文件。
2. 在本地隔离副本用同一主密钥演练14→15，核对旧账号密文关联上下文 `account:<id>`、外部secret引用、索引/任务/人工保护不变。迁移失败事务回滚；重复启动幂等。
3. 发布后核对readyz/version/schema、健康/重启、旧邮箱连接、新授权、目录和专用任务。Oracle1仅部署运行，Compose位于`/opt/1panel/docker/compose/mail_auto`，继续使用生产latest；本次没有触碰。
4. 回退时停止新服务，恢复升级前数据库+同一主密钥+旧镜像。不能手改user_version或只换旧二进制。保留新库取证，核对供应商已执行动作，防止旧快照任务重放；回退不会自动撤销远端动作，新增授权/索引/操作记录也不会进入旧快照。

官方资料于2026-11-16重新获取，四次curl均退出0，本地HTML保存在`.planning/gmail-outlook-implementation/`的`google-xoauth2.html`、`google-extensions.html`、`google-lifecycle.html`、`microsoft-oauth.html`；不把协议支持当作项目实测：

- [Gmail XOAUTH2](https://developers.google.com/workspace/gmail/imap/xoauth2-protocol)
- [Gmail IMAP 扩展](https://developers.google.com/workspace/gmail/imap/imap-extensions)
- [Google OAuth 生命周期与 Testing](https://developers.google.com/identity/protocols/oauth2)
- [Microsoft IMAP OAuth 个人与组织账号](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)

## 11. 风险、资源与后续责任

- 外部阻塞：真实应用/固定HTTPS/本人同意/专用测试邮件缺失；两个个人服务商不能宣告通过，组织账号始终未验证。
- 范围待决：Gmail Trash/恢复和特殊目录动作安全拒绝；主代理需决定受限范围是否可接受，若要求补齐须先取得真实测试条件并再实施/复验。
- Google Testing期限、restricted scope政策、Microsoft secret期限及组织同意/IMAP策略可能影响持续可用性，不能承诺永久授权。
- 本地模拟证明安全分支、并发和协议预期，不能覆盖服务商所有限流、标签传播延迟、别名、租户及全链路长时间行为。主代理应独立构造反例，而不是仅接受本报告。
- 本次没有收到真实凭据或私人邮件；OAuth/IMAP测试仅合成数据。UI临时实例、随机bootstrap凭据和t.TempDir夹具随测试清除；旧二进制兼容夹具在实验后删除。现有他人工作文件保留，临时诊断/日志不入Git。
- `docker buildx rm mail-auto-oauth-validation`已退出0，移除仅供本任务使用的构建器，证据`validation/builder-cleanup.log`；未清理任何生产/其他项目资源。保留本地OCI归档和脱敏证据供主代理验收，均排除Git/镜像；当前`bin/mail-auto`为本地0.10.0构建。没有调用REA、启动REA MCP/CLI、委派子代理或访问Oracle1。

交接顺序保持：固定代码候选 → 本报告 → 主代理独立审查/重跑/真实证据核验 → 修复问题 → 主代理复验 → 发布部署。实施协作者没有自行宣布独立验收通过。**等待主代理独立验收；尚未发布部署。**

## 12. GO-IA-01～04 返工与复验交接（2026-11-16）

本节为本轮最新交接。独立报告 `docs/gmail-outlook-independent-acceptance.md` 的“不通过”结论保持原样；本节仅记录实施协作者修复和自测，不宣告独立复验通过。

### 12.1 基线、候选及范围

返工基线 HEAD 为 `ace3a1055bdf0765052ab5bfdbded04bf60e79ed`，初始已跟踪内容无差异。保留原有未跟踪 `.planning/`、`AGENTS.md`、实施方案、协作者提示和独立验收报告。版本继续使用尚未发布的 `0.10.0`，schema 仍为 `15`：本次使用已有冻结 JSON 保存的稳定身份并为新摘要增加明确前缀，无新表/列/DDL，不占用或改写迁移，不创建或覆盖任何发布标签。

新代码候选为 **`574b04742ef4b783cb7333de97d2f282ea722bcd`**，tree 为 `363dcea7e674a4adab30769bc5b85913fba72dcf`，共 18 个文件（625 行增加、21 行删除）。候选暂存逐文件哈希/路径/凭据模式检查退出 0，凭据命中 0；没有纳入原未跟踪资料、`.planning/`、密钥、邮件数据或私有部署配置。后续交接文档提交仅更新本报告及新增两份返工测试/镜像 JSON，不改候选源码。工作流仍只有检查及 `push: false` 镜像构建；本轮仅本地提交，不推送，不发布标签/镜像，不访问或部署 Oracle1，不清理生产资源。

18 个文件逐项列于 `docs/gmail-outlook-rework-test-results.json` 的 `source_candidate.staged_files`；其中实现文件见下表，另外包括 `CHANGELOG.md`、`docs/gmail-outlook-setup.md`、正式回归/夹具、默认 UI 断言、MCP 清单断言及 `scripts/gmail-outlook-rework-verify.py`。本轮未修改 go.mod、依赖、认证 scope、IMAP 协议写入、任务批量或生产保护设置。

### 12.2 四项修复与正式回归

| 问题 | 修复及源码 | 正式回归 |
| --- | --- | --- |
| GO-IA-01 / P1 | `gmail.go` 的 `logicalPending` 从 operation_items → operations → 冻结 operation_plans 按位置读取账号和字符串 Gmail ID；`logicalClaims` 从 automation_claims → 冻结 automation_batches 读取同一身份。`automation.go` 的可领取检查、事务内领取复查、`operations.go` 的手动排队均复用持久身份，不依赖当前 messages 行。运行领取表本来已持久保存 Gmail ID，继续保留。 | `TestGmailPersistentPendingFence` 覆盖 queued/running/needs_verification、源行删除、别名重新索引及 UIDVALIDITY 改变、第二 SQLite 连接/新 Service、两个实例并发入队、未结案保持 busy 和结案释放；`TestGmailPersistentAutomationClaim` 覆盖领取批次在源行消失后的手动/自动冲突和过期释放；`TestGmailClaimAIAfterSourceRemoved` 覆盖真实 ClaimAI 入口。 |
| GO-IA-02 / P1 | `operations_recovery.go/UndoPlan` 在生成任何补偿计划前统一拒绝 Gmail 移动/归档撤销，返回固定 `gmail_undo_unsupported` 及中文行动说明。普通移动、归档和标记能力未扩大。`folder_settings.go`、`automation_policy.go`、MCP mail_undo 描述、Agent 固定提示、UI 授权说明及操作列表说明均展示边界。 | `TestGmailMoveUndoFailsClosed` 覆盖目标原有/新增、用户后续改标签、缺少/过期证据、未知部分写入及归档，断言不生成或保存反向计划；同时核对规则/任务能力声明。MCP 工具清单测试核对拒绝说明；默认 OAuth UI 测试在三种宽度断言撤销限制。 |
| GO-IA-03 / P2 | `operations.go` 新摘要使用 `v2:`；`recoveryIdentityMatches` 为旧七字段和 schema14 五字段提供受限只读兼容。旧计划不能通过 ExecutePlan 入队；已持久排队操作启动时配置校验安全失败。`finishRecoveredMove` 明确拒绝无版本摘要的清理写入。只读核验前后检查当前完整快照，原计划不自动改写。 | `TestSchema14PersistedRecoveryUpgrade` 从原 0.9.2 实际导出的八张相关表布局及旧 JSON/摘要创建 schema14 库，再经正常启动升级、重复打开；覆盖未改变、邮箱身份/文件夹/网络/OAuth/管理员授权变化、只读成功、拒绝清理及旧排队拒绝。`TestUnversionedCandidateRecoveryIsReadOnly` 覆盖首轮 schema15 七字段候选；`TestLegacyManualKeepReadOnlyCompatibility` 覆盖旧直连人工结案及网络变化拒绝。 |
| GO-IA-04 / P2 | `manual_resolution.go/ResolveManualKeep` 在远端读取前保存完整当前账号；确认事务调用 `accountNetworkIn` 补全 OAuth、账号网络策略及路由，再比对完整 v2 快照及原冻结摘要。网络读取期间没有 SQLite 事务。 | `TestManualKeepProductionNetworkSnapshot` 使用生产 `network.New(Store,Vault)`，直连/代理各覆盖未变、身份变化、预览后网络变化、确认读取期间网络变化；未变结案成功，变更拒绝且无结案落库、无额外远端写入。 |

正式测试位于 `internal/mail/acceptance_regression_test.go`。schema14 静态夹具和来源说明位于 `internal/mail/testdata/schema14-recovery.sql`、`schema14-recovery.md`：由原基线 `organizerFixture`/不确定移动/冻结计划真实导出，保留相关表精确旧布局和五字段摘要；仅合成数据，归一化 ID、延长测试有效期、移除密文，测试时创建临时 Vault 密文。没有从新版结构假算一个旧摘要来替代迁移夹具。无关空表由正常启动补建；这不是生产数据库升级证据。

### 12.3 历史摘要与升级处理策略

| 持久摘要 | 允许的核验/本地结案 | 新写入/恢复清理 |
| --- | --- | --- |
| schema14 无版本五字段 | 必须精确匹配 provider/username/host/port/folders，必须仍是旧密码服务商、无 OAuth，当前有效路由为空（无 Manager 的旧夹具）或 direct；生产 Manager 为 direct。仅只读核验及符合完整原文证据的本地人工保留结案；读取前后完整快照再次相同。 | 一律拒绝；旧预览须重建，旧排队安全失败，待核验记录保留。 |
| 首轮候选无版本七字段 | 必须精确匹配全部七字段（包括 NetworkRevision/OAuth）；只读核验和本地结案，确认读取前后比较完整 v2 快照。 | 一律拒绝，返回可行动提示。 |
| 新 `v2:` 摘要 | 绑定原五字段、有效路由、OAuth 代际/配置和账号网络策略，完整比较。 | 沿用现有权限/任务/设置/保护/未知结果控制；Gmail 恢复标签写入及移动撤销继续拒绝。 |

五字段历史摘要没有记录过去的网络证据，不能声称已证明升级前出口不变。对当前代理账号不猜测、不自动切直连，不无证据批量更新摘要，保持待核验并提示人工核对；这是明确的保守兼容限制。只读确认不需要继续原清理授权，任何真正清理仍要求 v2 以及全部既有权限/任务/规则/设置/取消/保护检查，未跳过这些检查。人工结案只落本地完整证据和保护，不清理源副本、不伪造映射。

schema15 布局未改变，但旧未发布候选二进制不了解新摘要及新保护，不应与本候选混用 worker。生产升级仍须备份数据库、配套 master.key、外部 secret 和私有配置，在隔离副本演练；回退原 schema14 仍须恢复配套数据库/主密钥/旧镜像，不能手工降 user_version。远端已执行动作不会随数据库回退撤销，启用任务前须核对未决操作。

### 12.4 新测试证据与开发失败

最终源码全包 race 于 12:37:18（+08:00）开始，291.463 秒完成，退出 0，**558 叶子通过 / 0 失败 / 0 跳过**，含父级 618 个结果事件。8 个新增正式测试包含 31 个叶子场景，均进入实际默认 `go test ./...`。其他门禁使用最终源码，执行于 12:36–12:40；原四反例在最终源码上再次复跑，退出 0、4/0/0。

新入口 `MAIL_AUTO_TEST_BUILDER=mail-auto-rework-validation python3 scripts/gmail-outlook-rework-verify.py` 将结果写入独立 `docs/gmail-outlook-rework-test-results.json` 与 `.planning/gmail-outlook-rework/validation/`，不覆盖初次实施或独立验收产物。先执行 `docker buildx create --driver docker-container --name mail-auto-rework-validation --bootstrap`（本轮退出 0），镜像完成后仅移除本次 builder（退出 0）。首次包装入口实际退出 **1**：包含当时未修正的 ClaimAI 夹具失败以及原有全目录格式异常。最终 race 单独重跑退出 0，已替换 JSON 当前 race 记录，首次失败保留在 `prior_attempts` 与原日志；没有把包装入口改记为 0。

下表路径均相对于 `.planning/gmail-outlook-rework/`。完整命令数组、起始时间、耗时、退出码、版本、计数和补充命令见 `docs/gmail-outlook-rework-test-results.json`；非测试检查不虚构通过数量。

| 实际命令 | 退出码 | 通过 / 失败 / 跳过或结论 | 新脱敏证据 |
| --- | --- | --- | --- |
| `go test -race -count=1 -json ./...` | 0 | 558 / 0 / 0 叶子测试 | `final-race.jsonl`、`final-race-result.json` |
| `go test -overlay=.planning/gmail-outlook-independent-acceptance/overlay.json -race -count=1 -json ./internal/mail -run '^TestIndependent'` | 0 | 4 / 0 / 0，原断言原样执行 | `independent-final.jsonl`、`independent-final-result.json` |
| `go vet ./...` | 0 | 无诊断 | `validation/vet-20261116-123647.log` |
| `gofmt -l .` | 0 | 列出 3 个原有临时文件，格式断言仍非绿 | `validation/format-20261116-123649.log` |
| `gofmt -l cmd internal scripts` | 0 | 未格式化源码 0 | `validation/source-format-20261116-123649.log` |
| `git write-tree` → `git archive` 候选 tree → 干净导出内 `gofmt -l .` / 空输出断言 | 均 0 | 1 / 0 / 0，候选全目录未格式化 0；临时导出已移除 | `clean-candidate-format.json` |
| `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...` | 0 | 无诊断 | `validation/staticcheck-20261116-123649.log` |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 0 | 可达符号漏洞 0，另有未调用的 2 个包级 / 1 个模块级结果 | `validation/govulncheck-20261116-123653.log` |
| `python3 -m unittest discover -s scripts/release-verify -v` | 0 | 46 / 0 / 0 | `validation/release-fixtures-20261116-123700.log` |
| `docker compose config --quiet` | 0 | 配置通过 | `validation/compose-20261116-123703.log` |
| `make build`；`bin/mail-auto version` | 均 0 | 本机构建完成，输出 0.10.0 | `validation/build-20261116-123703.log`、`extra-checks.json` |
| `scripts/ui-check/run.sh` | 0 | 1017 / 0 / 0 断言；18 个脚本/宽度组合；页面错误 0；OAuth 32×3=96 | `validation/ui-20261116-123705.log` |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` | 0 | 27 / 0 / 0 | `validation/ui-gate-20261116-123807.log` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o .planning/gmail-outlook-rework/mail-auto-amd64 ./cmd/mail-auto`；随后 `file` | 均 0 | 静态 Linux x86-64 ELF | `extra-checks.json` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o .planning/gmail-outlook-rework/mail-auto-arm64 ./cmd/mail-auto`；随后 `file` | 均 0 | 静态 Linux ARM aarch64 ELF | `extra-checks.json` |
| `docker buildx build --builder mail-auto-rework-validation --platform linux/amd64,linux/arm64 --output type=oci,dest=.planning/gmail-outlook-rework/candidate-image.tar .` | 0 | 两架构本地 OCI 构建完成，无推送，无发布标签 | `validation/oci-20261116-123912.log` |
| Python tarfile / SHA-256 检查 OCI | 0 | 2 平台通过 / 0 失败 / 0 跳过；30 个 blob 摘要一致，manifest/config 平台匹配 | `docs/gmail-outlook-rework-image-build.json`（仓库相对路径） |
| `git diff --check`；`git diff --cached --check`；`git diff --check ace3a10..574b047` | 均 0 | 无空白错误，暂存 18 文件已审查 | `validation/diff-20261116-123912.log`、`candidate-precommit-scan.json`、测试 JSON 的 `candidate_diff_check` |

工具版本：Go 1.27.4 darwin/arm64；staticcheck 2026.2.2 (0.8.2)；govulncheck 1.9.0（库更新时间 2026-11-11 02:51:11 UTC）；Node 24.22.3；Python 3.9.6；Docker 29.8.2；Buildx 0.37.2；Compose 5.5.0；Chrome 157.0.8103.51；puppeteer-core 25.16.0、@puppeteer/browsers 3.2.6；Git 2.39.5 Apple Git-154。版本命令实际均退出 0。本轮没有运行远端 CI，未声称使用 CI 固定 Chrome156/Node24.14；本地默认门禁确实运行三宽度 Chrome157。

全目录格式异常仍是第 7 节列出的三个既有 `.planning/` / `.claude/worktrees/` 文件，未修改或删除它们以获得绿色。干净候选 tree 格式断言单独通过。govulncheck 的符号结果为 0 不等于所有依赖无漏洞。

本地 OCI 归档 SHA-256 为 `0bb01480587e3c5ca44ac7f8320971b6c95a277daad36e638463d98479efed3d`；amd64 manifest 为 `sha256:edc3882645e2461caab1536081a6cf1df988d96ae6d1730868b02501008a4c32`，arm64 manifest 为 `sha256:838dfe176cca0513ac19c1e0f2651328e074f51153bfc250639fa73528874990`。这些仅为本次本地构建证据，不是 registry/生产摘要；新 JSON 和 `.planning/` 归档均未覆盖旧产物。

已保留的开发记录：首批 28 叶子回归 race 通过；增加正式 ClaimAI 后，夹具先因目标与来源重叠被规则校验拒绝，之后因默认历史起点保护合成的未知收信时间而没有领取，均修正夹具到独立 QA 目标和明确 Since=0。没有修改生产保护、减少断言或跳过测试；首轮全量失败结果保留。原四个 independent overlay 反例在本轮已原样复跑，退出 0，4 通过 / 0 失败 / 0 跳过；最终证据另列。

具体失败计数：`targeted-2.jsonl` 为 31 通过 / 1 失败 / 0 跳过、退出 1；首轮 `validation/race-20261116-123155.log` 为 557 / 1 / 0、退出 1，均只有新增 ClaimAI 夹具失败。修正后该单项 race 为 1 / 0 / 0、退出 0，随后最终完整 race 为 558 / 0 / 0、退出 0。未发生本轮外部构建/网络失败；既有异常仅全目录格式诊断。独立原报告、probe 源码、overlay 及 `probes.log` 均保留，哈希索引见测试 JSON 的 `independent_original_hashes`；本轮新通过输出保存在新目录，未覆盖旧失败日志。

### 12.5 未完成项及外部步骤

该节最初记录时两个个人邮箱的真实验收均未启动；后续 Outlook 官方授权、IMAP诊断和重启后刷新路径已完成，详见第 13.5–13.6 节。当前真实写入仍为 0，没有真实 run/operation ID。Gmail真实账号验收待完成，Outlook QA 邮件同步和整理待专用数据准备；Workspace / Microsoft 365 仍为未验证兼容扩展。不要在聊天中提供密码、token 或 client secret。

当前 Gmail 移动/归档撤销为明确拒绝，若后续希望开放，须另行保存操作前完整成员关系、验证并发及用户后续修改、处理部分写入，再用专用真实测试数据验收。不得把反向 move 恢复为通用撤销。历史代理账号无网络证据的只读恢复限制也保留为需人工核对项。

**等待主代理独立复验；尚未发布部署。** 主代理应独立审查新候选差异，原样重跑四反例及正式迁移/并发/人工结案回归，核对新测试与真实账号证据；本协作者没有替主代理宣布通过。

## 13. OAuth 应用跨部署复用增量（0.10.1）

### 13.1 基线、范围和候选

用户要求 Google / Microsoft 都集成复用，避免每次部署重新创建应用。本轮从 `fbc1054a2490ecf86bc05f39eaadc688dcccdfe7` 继续，本地版本更新为 `0.10.1`，schema 仍为 `15`，没有新增或覆盖迁移、依赖、发布标签。`docs/gmail-outlook-independent-reacceptance.md` 已记录 `574b047` 的四项修复代码复验通过、整体部分通过、生产发布不通过；本次没有修改该报告或原失败证据。

本轮新增改动尚未提交，候选提交为“无”。基线加工作区改动构成本轮待审候选；22 个实现/使用文档文件的逐文件 SHA-256 固定在 `docs/oauth-app-reuse-test-results.json` 的 `candidate_files`，交接报告和结果 JSON 不计入该实现快照。未暂存、提交、推送、发布镜像或访问 Oracle1/Oracle3，未部署或清理生产资源。保留原先未跟踪的方案、验收材料、`AGENTS.md` 和 `.planning/`；没有把这些混入本轮实现。

| 文件 | 实现说明 |
| --- | --- |
| `internal/mailoauth/deployment.go`、`deployment_test.go` | 版本 1 私有 JSON 部署文件，最多两服务商/32 KiB；拒绝符号链接、非普通文件、组/其他用户可读文件、未知字段/服务商、额外 JSON 文档、过期/空密钥；两家应用在同一短事务内载入，目标 Vault 重新加密；安全导出不覆盖已有文件、0600，不导出邮箱 token |
| `internal/mailoauth/manager.go`、`model.go` | 复用同一应用验证/更新函数；启动导入相同配置无副作用；密钥轮换保留 refresh，应用身份变化要求重授权；导入失败使用固定安全错误，不包含供应商或数据库敏感正文 |
| `internal/config/config.go`、`config_test.go` | 新增 `mail_oauth_apps_file` / `--mail-oauth-apps-file`，要求 HTTPS public_url，保持 CLI 优先和 MCP 独立性 |
| `cmd/mail-auto/main.go`、`main_test.go` | `mail-oauth export PATH` 管理命令；启动在同步、恢复 worker 和 HTTP 服务前导入；错误文件拒绝启动 |
| `internal/admin/web/index.html`、`scripts/ui-check/mail-oauth-ui-check.js` | 提示应用注册一次、多邮箱分别官方登录、新域名需登记回调；新增断言进入默认三宽度门禁 |
| 版本、Compose、配置示例、README、CHANGELOG、接入指南、发布核对脚本及其夹具 | 统一 0.10.1/schema15；不包含真实应用凭据；新增 `docs/oauth-app-reuse.md` 说明导出、挂载、轮换和回退 |

同一个管理员注册的服务商客户端可在自用实例复用，不新增共享 secret 到公共源码/镜像，也不新增授权中转服务。每个邮箱仍分别完成官方同意；导入文件不携带邮箱身份、refresh/access token、主密钥或 origin。文件只作为启动配置来源，不作为整个数据库备份。用户管理自己的应用这一原方案边界保留；本轮补充安全迁移/自动配置能力，不把“可复用”说成“任意全新第三方安装都可零配置登录”。

### 13.2 测试与构建证据

原始证据根目录为 `.planning/oauth-app-reuse/`。每条命令的实际 argv、时间、耗时、退出码、计数和日志路径见 `docs/oauth-app-reuse-test-results.json`；下表非测试检查不虚构测试数量。既有全量测试后只补充证据汇总和本交接，未再修改生产源码。

| 命令或检查 | 退出码 | 结果 | 证据（根目录内） |
| --- | --- | --- | --- |
| `go test -race -count=1 -json ./...` | 0 | 587 / 0 / 0 叶子测试 | `full-race.json`、`.log` |
| 导入/导出、配置、CLI 定向 race（完整 `-run` 见 JSON） | 0 | 29 / 0 / 0；含跨 Vault、相同配置重启、secret/origin/client/authority/expiry 变化、两家原子失败回滚、未提供服务商保持不变及文件边界 | `targeted.json`、`.log` |
| 原四个 independent overlay 反例 race | 0 | 4 / 0 / 0；未改原断言 | `original-counterexamples.json`、`.log` |
| `go vet ./...`；固定版本 staticcheck；固定版本 govulncheck | 均 0 | 无静态诊断；可达符号漏洞 0，仍有未调用的 2 个包级、1 个模块级结果 | `vet`、`staticcheck`、`govulncheck` 的 `.json` / `.log` |
| `gofmt -l cmd internal scripts` 的空输出断言 | 0 | 未格式化源码 0 | `source-format.json`、`.log` |
| `gofmt -l .` 的空输出断言 | 1 | 仍有原有三个临时 Go 文件未格式化，未修改它们 | `workspace-format.json`、`.log` |
| `python3 -m unittest discover -s scripts/release-verify -v` | 0 | 46 / 0 / 0 | `release-fixtures.json`、`.log` |
| 默认及示例 override 的 `docker compose ... config --quiet` | 均 0 | 两份配置通过；可选 secret 示例未提供真实密钥 | `compose`、`compose-example` 的 `.json` / `.log` |
| `make build`；`bin/mail-auto version` | 均 0 | 本机构建成功，版本 0.10.1 | `build.json`、`.log`；版本另于交接检查时实际输出 |
| `scripts/ui-check/run.sh` | 0 | 1020 / 0 / 0 断言，六脚本×三宽度，页面错误 0；OAuth 33×3=99 | `ui.json`、`.log` |
| UI gate 首次与默认 UI 并行；随后隔离重跑 | 1 → 0 | 首次 26 / 1 / 0；隔离 27 / 0 / 0 | `ui-gate`、`ui-gate-isolated` 的 `.json` / `.log` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64/arm64 go build -trimpath ...` | 均 0 | 两架构编译通过 | `linux-amd64`、`linux-arm64` 的 `.json` / `.log` |
| `docker buildx build --platform linux/amd64,linux/arm64 --output type=oci,... .` | 0 | 仅本地 OCI，两平台 manifest/config 和 32 个 blob 摘要通过，无 push | `multiarch-image.json`、`.log`、`image-verification.json` |
| 创建/移除本轮临时 builder | 均 0 | 仅处理 `mail-auto-oauth-reuse-0101` | `builder-create`、`builder-cleanup` 的 `.json` / `.log` |
| `git diff --check` | 0 | 无空白错误 | `diff-check.json`、`.log` |
| 实际 Google 应用导出及临时新 Vault 实例启动、重启 | 0 | 两次就绪均 0.10.1/schema15，Google 应用版本均 1、密钥加密、邮箱账号 0 | `local-google-export.json`、`local-google-reuse-smoke.json` 及关联日志 |

工具实际版本：Go 1.27.4 darwin/arm64；Node 24.22.3；Python 3.9.6；Git 2.39.5 Apple Git-154；Docker 29.8.2；Buildx 0.37.2；Compose 5.5.0；Chrome 157.0.8103.51；puppeteer-core 25.16.0、@puppeteer/browsers 3.2.6；staticcheck 2026.2.2 (0.8.2)；govulncheck 1.9.0，库更新 2026-11-11 02:51:11 UTC。版本记录在 `versions.json`，两项 Node 依赖由 `scripts/ui-check` 解析验证。未运行远端 CI。

首次 UI gate 失败断言是 `missing pass exits non-zero` 的全局临时目录/进程计数：并行默认 UI 正好清理自己的实例，前后计数从 1/2 变成 0/0。隔离运行同一测试全部通过，没有改断言或跳过测试。原工作区格式异常仍为第 7 节列出的三个临时文件，不能称所有工作区门禁全绿。辅助汇总最初假定 UI JSON 使用 `results` 字段而失败，已按实际 `out/total` 格式核对 18 项并修正；另一次在仓库根查 Node 依赖版本失败，改从 `scripts/ui-check` 解析成功。这两次采集错误没有改变测试结果。

本地 OCI SHA-256 为 `6a4b89e87a57c96daa6c166d7c571638a0f09d6f5f1c4aa5b027e70756fa9bf9`；amd64 manifest `sha256:fcc628b69c8f5b70a8543dc8b8f5ec15a10e99934ddb51bbcd24892092410036`，arm64 manifest `sha256:03ae0416359c516a910e4fac5d5481d91dfbc1b8f33b3e2d0be14658302b43be`。仅为本地构建证据，不是 registry/生产摘要。

最终交接检查 `final-review.json` 退出 0：22 个实现文件哈希与测试快照一致，全部日志路径存在，源码格式和差异检查通过，构建版本 0.10.1。私钥、Google token/secret 字面量等有限模式扫描命中 0，并已人工审查差异；该模式检查不声称穷尽全部凭据格式。真实应用导出文件仍在仓库及构建上下文之外。

### 13.3 实际配置、限制和最少外部步骤

| 项目 | 当前状态 |
| --- | --- |
| Google 应用复用 | 本机 QA 中已配置的真实客户端完成私有导出，临时实例两次启动导入通过；文件保存在仓库外受保护的 MailAuto-QA 目录，未打印凭据 |
| Microsoft 应用复用 | 用户已注册个人账号应用并配置受保护的本机 Vault；应用复用模拟见前述证据 |
| Gmail / Outlook 真实邮箱 | Outlook 官方授权、IMAP诊断和重启后真实 refresh 路径已通过；Gmail未开始；两者同步、专用邮件写入和受控规则均未完成；真实写入 0，无真实 run/operation ID |
| 组织账号 | Workspace / M365 均未验证 |
| Gmail 功能边界 | 第 12 节的 Trash/恢复、特殊目录、保留两份、移动/归档撤销等拒绝策略不变 |
| 发布与部署 | 新增 0.10.1 改动尚待独立代码审查；真实验收仍阻塞发布，没有发布部署 |

最少用户步骤：在 Outlook 网页创建 `MailAuto-QA` 和 `MailAuto-QA-Target` 文件夹，在前者准备明确非私人测试邮件；然后通知实施者继续验收。Gmail本人仍需在官方页完成邮箱同意并准备专用标签和测试邮件。不要把密钥发到聊天。管理员可重新导出两家配置后，以只读文件载入后续自用实例。新域名在两家原应用补登记精确回调，地址见 `docs/oauth-app-reuse.md`；同域名重建且保留数据库/主密钥无需重新创建客户端。Google Testing 限制、Microsoft 凭据到期及组织策略继续适用；不能保证所有部署永不需要管理应用。

私有文件 0400/0600、父目录建议 0700，容器读取用户 65532；不进入 Git、镜像、公开备份、截图或聊天。启动导入不发起供应商网络请求，既有 OAuth/token/IMAP 网络策略和端点白名单不变。相同文件重启不增加版本或清除 state；同客户端轮换密钥保留 refresh，client ID/origin/账号类型变化要求重授权。启用文件后在 UI 改配置，须同步重新导出或移除文件配置，避免下次启动恢复旧值。

升级和回退：无新 schema，仍须先备份数据库及配套主密钥、私有文件。回退到含四项返工修复的 0.10.0 候选前停止进程、移除新参数/YAML 字段；不能回退至缺少 Gmail 排他/撤销等保护的初次候选。跨 schema14 回退继续恢复配套快照，不能手改 user_version。文件只能恢复应用配置，不能代替邮箱授权、任务和操作记录备份，也不能撤销远端已完成动作。

**模拟测试完成，真实账号验收待完成。等待主代理独立验收；尚未发布部署。**

### 13.4 Microsoft 实际应用配置补充

第 13.3 节“Microsoft 尚待注册”为此前交接状态。其后用户完成 Azure 注册并确认创建个人账号应用及 24 个月密钥，现已保存到本机 QA 的加密配置，门户到期日 2028-11-15。Google/Microsoft 两家配置已一起私有导出，并在隔离新 Vault 实例及重启中验证自动导入成功、配置版本均保持 1，命令退出 0；证据为 `.planning/oauth-app-reuse/both-provider-export.json`、`both-provider-reuse-smoke.json` 及关联日志。

应用配置导入验证本身没有完成邮箱同意或 IMAP/refresh；此后用户完成 Outlook 官方同意、动态 IMAP scope、真实连接和重启后刷新路径，详见 13.5–13.6。真实写入仍为 0。没有申请 Graph 邮件权限。后续部署采用私有只读文件自动载入，密钥不内置到公共源码、镜像或 Compose。未发布部署，原独立代码验收边界不变。

### 13.5 个人 Outlook 授权与连接实测补充

2026-11-16 16:34（Asia/Shanghai），MS-PERSONAL-01 在本机运行中的 0.10.0/schema15 实例完成本人官方同意、回调绑定及 IMAP 连接诊断，状态 connected，refresh 加密保存，实际 Outlook IMAP scope 存在。TCP/TLS/XOAUTH2 登录和 LIST 全部通过，共读取 11 个文件夹，直连且无回退。首次同意后曾保持 pending，重新官方登录确认成功；首次失败原因未确定，没有把失败改记为成功。

同步范围为 MailAuto-QA、自动同步关闭；邮件索引 0、任务 0、真实写入 0。证据 `.planning/oauth-app-reuse/outlook-authorization-imap.json`、`outlook-imap-success.jpg`，详情见真实验收文档相应小节。新目录缺少 Exchange Online 静态列表没有阻止此次个人账号动态 IMAP scope 授权，未添加 Graph 邮件权限。尚未验证实际 refresh grant、同步、重启恢复和专用邮件操作，Gmail 真实链路仍待验；这不是 0.10.1 新增代码的独立验收或生产发布授权。尚未发布部署。

### 13.6 个人 Outlook 重启后刷新与连接实测

2026-11-16 18:53（Asia/Shanghai），在隔离本机 QA 的 0.10.0/schema15 实例停止并重启 Mail Auto，以清空进程内 OAuth access-token 缓存；重启后立即运行 Outlook 连接诊断。TCP、TLS、XOAUTH2 登录和 IMAP LIST 均通过，读取 11 个文件夹，直连且无自动同步。依据 `internal/mailoauth/manager.go` 的 credential 路径，缓存为空时会发起 refresh-token grant；连接诊断在重启后成功，因此确认此次真实 provider refresh 路径取得可用 access token。没有保留 provider 侧审计事件或原始 token 响应，也不声称 Microsoft 轮换了 refresh token。摘要见 `.planning/gmail-outlook-real-acceptance/outlook-refresh-after-restart.json`。

本次索引 0、任务 0、真实写入 0。Outlook 网页当前没有 `MailAuto-QA` 或 `MailAuto-QA-Target` 文件夹及专用测试邮件，尚未执行同步或整理。下一步等待用户在个人 Outlook 中创建这两个文件夹并准备明确非私人测试邮件，再限制同步范围、执行受控操作并对照网页状态。Gmail真实验收仍未完成；个人Outlook真实验收仅完成官方授权、IMAP诊断及重启后刷新路径。**等待主代理独立复验；尚未发布部署。**

### 13.7 Outlook QA 同步受阻

2026-11-16 19:04（Asia/Shanghai），用户告知测试数据已准备后，隔离实例仅对配置的 `MailAuto-QA` 发起手动同步；0/1 文件夹完成，页面提示服务器拒绝打开文件夹或返回无效响应，原索引保留。当前 Outlook 网页登录的邮箱与隔离实例已授权邮箱不同，无法核对专用目录和邮件是否在目标账号；失败原因尚未确定，不能断言文件夹不存在。等待用户切换网页至已授权邮箱以确认。没有执行整理或写入，Gmail与 Outlook 完整真实验收仍未完成，尚未发布部署。

## 14. Outlook QA 目录同步排查与诊断修复（0.10.2）

### 14.1 基线、原因和改动

本轮 HEAD 仍为 `fbc1054a2490ecf86bc05f39eaadc688dcccdfe7`，已有未提交 0.10.1 应用复用、真实验收文档等工作全部保留。本轮版本提升为未发布 0.10.2，schema15 无新迁移；运行时、管理界面、Compose及当前配置/接入文档版本一致。没有新标签或推送，新候选为工作区差异和测试快照哈希，见 `docs/outlook-folder-sync-test-results.json`。独立验收报告及原反例不修改。

用户确认在 MS-PERSONAL-01 准备 QA 邮件后，19:18 使用该本地账号 Vault、原网络策略做真实只读 LIST/EXAMINE：11个目录中无 MailAuto QA 目录，EXAMINE 明确返回目录不存在。此前从网页另一个账号推断数据准备账号有误缺乏充分证据，本轮不沿用该推断。真实失败为“指定目录当前在 IMAP 不可见”，不是 OAuth/IMAP 认证失败；客户端目录是否在服务器、嵌套路径或服务商可见性仍待外部核对。没有自动创建目录或扩大范围来掩盖问题。

| 文件 | 修复 |
| --- | --- |
| `internal/imap/client.go` | 提取同一连接的可选目录 LIST；只读 EXAMINE 普通拒绝后核对实际目录。缺失或 Noselect 返回安全错误码 `folder_unavailable` 和完整路径提示；存在、LIST失败保留原错误。成功路径无额外 LIST，网络失败不追加命令。 |
| `internal/imap/automatic.go` | 手动、自动增量、IDLE共用上述只读核验；不影响写入及未知结果处理。 |
| `internal/admin/web/network.js` | 诊断结果新增完整IMAP路径折叠区，目录名称全部转义显示。 |
| `internal/imap/folder_sync_test.go` | 真实TCP协议覆盖三入口、缺失/嵌套/不可选/存在但拒绝/LIST失败、网络中断、INBOX大小写；SQLite持久任务失败保留旧索引、重启后目录恢复可重试；断言无写入、无扫描其他目录和无盲重试。 |
| `scripts/ui-check/mail-oauth-ui-check.js` | 实际默认门禁新增完整路径/提示、HTML注入及展开后宽度断言。 |
| 版本、Compose、发布核对脚本/夹具及文档 | 当前0.10.2/schema15；兼容发布核对原schema14→15，原版本分支和断言保留。 |

### 14.2 本轮测试

完整命令、工具版本、退出码、叶子测试计数、日志和源文件哈希在 `docs/outlook-folder-sync-test-results.json`。脱敏日志根为 `.planning/outlook-folder-sync/`，不得提交临时probe、私有备份、截图或本地镜像。

| 命令 | 实际结果 / 证据 |
| --- | --- |
| `go test -race -count=1 -json ./...` | 退出0，604通过/0失败/0跳过；`race.log`。全量开始后新增的INBOX大小写单例随最终定向回归另行通过。 |
| `go test -race -count=1 -json ./internal/imap -run '^TestSyncFolder'` | 最终退出0，18叶子通过/0失败/0跳过（含父测试计19）；`targeted-final.jsonl`。首次夹具缺少登录后的CAPABILITY，7叶子通过/10失败，退出1；修正夹具后17叶子通过，再新增INBOX单例后18通过，未弱化断言。原失败日志均保留。 |
| 原四项 independent overlay，`-race -count=1 -json ./internal/mail -run '^TestIndependent'` | 退出0，4通过/0失败/0跳过；`independent.log`，原失败材料保持原样。 |
| `go vet ./...`、staticcheck@v0.8.2、govulncheck@v1.9.0 | 全部退出0；可达符号未检出漏洞不等于依赖无漏洞，govulncheck另提示未调用包/模块存在漏洞。 |
| `python3 -m unittest discover -s scripts/release-verify -v` | 退出0，46通过/0失败/0跳过；`release.log`。 |
| 源码格式、`git diff --check`、`docker compose config --quiet`、`make build` | 全部退出0；本机构建版本0.10.2。全目录gofmt断言因三个既有 `.planning/`、`.claude/worktrees/` 临时文件失败，未改动它们。 |
| 默认6脚本UI，1280/375/390 | 退出0，18组、1029断言全部通过，0页面错误；OAuth每宽度36断言含本次3项；`ui.log`。 |
| UI gate、linux/amd64与linux/arm64编译 | 全部退出0；UI gate为27通过/0失败/0跳过，双架构编译成功；`ui-gate.log`、`linux-amd64.log`、`linux-arm64.log`。未推送镜像或二进制。 |
| 双架构OCI本地镜像 | 构建退出0，32个blob摘要全部通过，完整摘要见 `image-verification.json`；任务专用构建器已删除，不影响其他资源。首次汇总脚本因Python3.9无hashlib.file_digest退出1，替换流式sha256后完整校验通过，不是镜像构建失败。 |

### 14.3 真实结果、升级及剩余阻塞

仓库外私有目录已配套备份原QA数据库、master.key及0.10.0旧程序；确认无活动/待核验操作后，仅本机隔离QA重启为0.10.2，自动同步/MCP继续关闭、原账号与允许目录保持不变。生产和Oracle1/Oracle3均未操作。带显式CA验证的readyz退出0，为ready/0.10.2/schema15。

真实重试仍0/1目录、0封；页面已显示明确的目录不可见提示，诊断TCP/TLS/XOAUTH2/LIST再次全部通过、11目录但无QA。OAuth connected、索引0、operation0、真实邮件写入0。证据 `live-list-examine.json`、`live-candidate-result.json`、`qa-folder-error.png`；JSON摘要只用MS-PERSONAL-01代号，截图为本地私有诊断界面、不提交。没有保存token、原始服务商错误或私人邮件正文。此次没有run/operation ID可交付，不能把诊断修复或模拟回归称为真实同步/整理通过。

最小外部步骤：本人在目标账号Outlook网页确认两个QA目录已保存到服务器；若是嵌套目录，提供目录的完整路径即可，不发邮件正文或凭据。之后仅按明确QA路径同步/整理；若网页已存在而IMAP继续不列出，继续排查目录可见性，不能推断为用户没准备。Gmail完整真实验收、Outlook真实邮件同步/整理/重授权及受控规则仍未完成，组织账号未验证。

回退本机诊断修复：停止QA，换回私有备份旧程序后重启；无新schema，原OAuth/账号不变，不需要手工改数据库。若要恢复整个QA快照，必须停服并恢复配套数据库和主密钥；不能把备份密钥放进Git/镜像。回退旧程序会恢复笼统错误提示，不能消除服务端目录不可见阻塞，也不能退回缺少GO-IA修复的初次候选。

**等待主代理独立验收；尚未发布部署。** 本轮仅本机QA用于验证；没有提交推送、版本标签、发布镜像、远端部署或生产清理。

### 14.4 继续排查：专用服务端目录补齐及真实同步通过

用户授权通过Chrome检查目标个人Outlook并修复至同步成功。19:37之后确认官方网页的邮箱根节点与已授权MS-PERSONAL-01一致，服务端目录树同样无MailAuto-QA，与之前IMAP LIST结果一致。在该账号根目录创建精确名称的专用目录后，19:39:08本机QA空目录手动同步完成（0封、1/1）；据此确认此次阻塞是配置目录未在服务端存在。之前失败证据不变，不再需要用户反复重新授权。

为验证非空同步，仅向空的MailAuto-QA通过IMAP APPEND准备三封纯合成测试邮件：M-MOVE-01已读无旗标、M-UNREAD-01未读、M-STAR-01已读有旗标，正文仅测试说明，使用`.invalid`固定地址、不外发。19:41:03首次非空同步和19:41:39重复同步均为completed、3封、1/1目录、错误为空。Outlook网页对应三封、1未读；数据库只读核对11项全部通过，未读/旗标保留、无重复索引、operation0。同步记录按账号持久化，无独立run ID；未执行整理，不存在整理operation ID。

| 命令或观察 | 实际结果与证据 |
| --- | --- |
| `go run .planning/outlook-folder-sync/seed_qa.go` | 退出0；仅APPEND三封专用合成邮件，外发0；`qa-preparation.json`。临时准备程序不提交、不进入产品或镜像。 |
| Chrome 手动同步三次 | 空目录一次及三封夹具两次均完成；`qa-sync-success.png`、`live-sync-success.json`，保留原失败截图与JSON。 |
| `python3 .planning/outlook-folder-sync/verify_live_sync.py` | 退出0，11通过/0失败/0跳过，Python3.9.6；证据`live-sync-success.json`，不读取或记录凭据/私人正文。 |
| `go test -race -count=1 -json ./internal/imap -run '^TestSyncFolder'` | 退出0，18叶子通过/0失败/0跳过，Go1.27.4；`targeted-live-confirmation.json`及JSONL。第14.2节全量门禁仍对应当前产品源码，本次仅调整QA数据及材料。 |

本次源目录创建1个、合成邮件APPEND3封、外发0、私人邮件打开/修改0、整理写入0；未修改产品源码、账号允许目录、网络策略、自动同步或MCP开关。版本保持0.10.2/schema15，HEAD仍fbc1054，未固定新提交；此前未提交变更全部保留。待验事项：目标QA目录准备及Outlook分类/归档/回收站/恢复/标记/规则保护/重新授权、Gmail完整真实验证、组织账号验证和新增增量独立复验。没有发布部署。**等待主代理独立验收；尚未发布部署。**

## 15. 固定 0.10.2 候选、Gmail 安全修复及本轮真实验收交接

本节是 2026-11-16 晚间的正式增量交接，取代第 13–14 节中描述当时状态的“未提交”“未注册”“未准备”等措辞。原独立验收、复验和 worker 后审计报告及原 overlay/反例/失败日志未修改。实施者只报告实现、自测和实际服务商结果，**不代替主代理宣布独立复验通过**。

### 15.1 固定边界、版本与文件

| 项目 | 本轮实际值 |
| --- | --- |
| 开始时 HEAD / 分支 | `fbc1054a2490ecf86bc05f39eaadc688dcccdfe7` / `main` |
| 原始基线 / 原返工候选 | `15c7160827d1ae56d3e177f818c02c450c53697e` / `574b04742ef4b783cb7333de97d2f282ea722bcd`；四项返工已独立代码复验关闭 |
| **本次源码候选** | **`811ab7a8c72c00c11f6b314841e8506c0bdddb58`**，仅本地提交，未推送 |
| **源码候选 tree** | **`97d459d270bc28a35df8946fb74a247534772fc5`** |
| 版本 / schema | **0.10.2 / 15**；0.10.1 应用复用与 0.10.2 目录/认证修复统一进入该候选 |
| 迁移和依赖 | 本轮未新增或修改迁移，`SchemaVersion=15`；未改 go.mod/go.sum |
| 版本占用 | 21:26:57–21:26:58 `git ls-remote --tags origin 'refs/tags/*0.10*'` 退出 0、输出空；未创建标签 |
| 变更清单 | 33 个候选文件及每个 SHA-256 见 `docs/gmail-outlook-final-candidate-test-results.json` 的 `source_candidate.files` |
| 后续材料 | 本报告、真实验收记录及最终测试 JSON 另作材料提交，不改变上述已测源码；独立复验同时审查源码候选及后续材料 |
| 工作区保留 | 原有未跟踪 AGENTS、方案、协作者提示、三个独立报告及 `.planning/` 保留，未混入源码候选 |
| CI 与发布边界 | `.github/workflows/ci.yml` 只有检查、`push:false` 双架构构建；本轮未推送、打标签、发布镜像、访问 Oracle1、部署或清理生产资源 |

第 13 节的安全应用导入/导出和第 14 节的目录诊断均包含在这 33 个文件中。运行时、UI、MCP 共用版本来源，Compose、README、配置说明及发布核对夹具统一为 0.10.2；历史 0.10.1/0.10.0 记录保留其当时版本。用户明确要求保留的 Outlook 全目录范围 `[]`、自动同步关闭、网络策略、每批数量和保护未被还原为早期 QA 设置。

### 15.2 本轮 Gmail / OAuth 修复与正式回归

| 实现文件 | 行为及默认门禁中的回归 |
| --- | --- |
| `internal/mailoauth/manager.go`、`oauth_test.go` | `Bind` 遇明确 IMAP 拒绝、scope 缺失或需重授权直接失败，不保存本次 refresh、不覆盖旧授权；新账号仍待授权。`TestBindRejectsExplicitIMAPAuthorizationFailure` 覆盖两服务商、新/旧授权及三错误；临时网络故障保留原 `network_error` 恢复路径，另有专门测试。 |
| `internal/imap/client.go`、`internal/admin/mail_oauth.go`、`mail_oauth_test.go`、`internal/mailoauth/model.go`、`internal/admin/web/app.js` | Gmail 缺少 X-GM-EXT-1 返回 `oauth_gmail_capability_missing` 并拒绝本次绑定；CAPABILITY 查询的传输失败仍归网络错误。新增包装错误/明确拒绝/临时网络/成功分类断言；OAuth 默认 UI 增加中文能力提示断言。 |
| `internal/mailoauth/manager.go`、`internal/network/settings.go`、`oauth_test.go` | 绑定、刷新成功和永久失败写回，在短 SQLite 事务内重新核对路由并使用代际/配置/密文 CAS。全局设置或实际账号策略变化在同一事务递增 OAuth generation，即使最终出口相同也拒绝旧 worker；保留原 refresh。HTTP 请求仍在事务外，无 SQLite 长事务。两项正式测试覆盖 bind/refresh/invalid_grant、出口变化、同出口全局和账号策略变化、旧凭据/缓存保护。 |
| `scripts/ui-check/mail-oauth-ui-check.js` | 应用复用说明、完整 IMAP 路径/转义、Gmail 能力提示都进入默认六脚本三宽度门禁；当前 OAuth 脚本每宽度 37 项。 |

源码审查继续核对统一认证、连接诊断、后台/手动同步、任务、读取、移动/标记及恢复入口；未新增旁路认证或写入路径。Gmail Trash/恢复/特殊目录移动、保留两份、move/archive undo、未知标签写入自动补齐仍服务端拒绝。未知写入只核验，不取消记录或重放来获得通过。

### 15.3 最终源码门禁及实际数量

证据根目录 E 为 `.planning/oauth-final-handoff/`；下表时间全部为 **2026-11-16 Asia/Shanghai**。每条完整 argv、环境覆盖、起止时间、实际退出码、日志路径、工具版本和测试数量均在新的 `docs/gmail-outlook-final-candidate-test-results.json`；对应 `E/<名称>.json` / `.log` 保留原始证据。测试顺序执行，UI 与 gate 不并行。

| 命令 | 起止时间 | 退出码 | 实际结果；E 中证据名称 |
| --- | --- | --- | --- |
| `go test -race -count=1 -json ./...` | 21:11:59–21:16:43 | 0 | **628 / 0 / 0 叶子测试**，含父级 694 个结果事件；`race` |
| `go vet ./...` | 21:16:43–21:16:44 | 0 | 无诊断；`vet` |
| `gofmt -l .` 及空输出断言 | 21:16:44 | 0 / **1** | 仍列出原有三个临时 Go 文件；`format-all`，未修改它们 |
| `gofmt -l cmd internal scripts` | 21:16:44 | 0 | 输出空；`source-format`；21:26:58 最终复核同样通过 |
| `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...` | 21:16:44–21:16:46 | 0 | 无诊断；`staticcheck` |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 21:16:46–21:16:53 | 0 | 可达符号漏洞 0；仍提示 2 个未调用包级、1 个模块级结果；`govulncheck` |
| `python3 -m unittest discover -s scripts/release-verify -v` | 21:16:53–21:16:56 | 0 | **46 / 0 / 0**；`release` |
| 默认及示例 override 的 `docker compose ... config --quiet` | 21:16:56 | 均 0 | 两份配置通过；`compose`、`compose-example` |
| `make build`；`bin/mail-auto version` | 21:16:56 | 均 0 | 本机构建完成，输出 **0.10.2**；`build`、`binary-version` |
| 原 independent overlay 的 `go test ... -race -count=1 -json ./internal/mail -run '^TestIndependent'` | 21:16:56–21:17:02 | 0 | **4 / 0 / 0**，原 overlay/反例不变；`independent` |
| `scripts/ui-check/run.sh` | 21:17:02–21:18:04 | 0 | **1032 / 0 / 0** 断言，18 组，页面错误 0；`ui` |
| `PUPPETEER=puppeteer-core node --test --test-reporter=tap scripts/ui-check/gate-test.cjs` | 21:18:04–21:19:10 | 0 | **27 / 0 / 0**；`ui-gate` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64/arm64 go build -trimpath ... ./cmd/mail-auto` | 21:19:10–21:19:13 | 均 0 | 两架构编译；`linux-amd64`、`linux-arm64` |
| 创建专用 docker-container builder / 两架构本地 OCI / 移除本轮 builder | 21:19:13–21:20:28 | 均 0 | 无 push；31 个 blob 摘要、两平台 manifest/config 全匹配；`builder-create`、`multiarch`、`image-verification.json`、`builder-cleanup` |
| `git diff --check` | 21:20:28；21:26:58 | 均 0 | 无空白错误；`diff`、`diff-final` |
| 定向认证/网络/目录/XOAUTH2 race，完整 `-run` 见 JSON | 21:32:20–21:32:41 | 0 | **42 / 0 / 0** 叶子测试；`auth-network-folder-regressions` |
| 暂存差异、路径/实际凭据扫描、干净 tree 导出格式断言 | 21:28:24–21:28:27 后 | 均 0 | 33 文件；真实 QA app secret/refresh 仅内存比较，命中 0；干净候选 gofmt 空输出；`staged-credential-scan`、`staged-diff-check`、`precommit.json` |

UI 数量按实际 `out` 重算：每个宽度为 **37 + 45 + 63 + 87 + 99 + 13 = 344**，三宽度共 **1032**。不沿用上一轮独立报告的 1029，也不修改独立报告原文。父/叶事件、重复执行和模拟/真实结果不相加作为覆盖率；真实阻塞不藏在单测 skip=0 中。工作区全目录格式例外不能表述成所有工作区门禁全绿；源码和干净固定候选格式另行通过。

工具版本均实际执行、退出 0：Go 1.27.4 darwin/arm64，Git 2.39.5 Apple Git-154，Python 3.9.6，Node 24.22.3，Chrome 157.0.8103.51，puppeteer-core 25.16.0，@puppeteer/browsers 3.2.6，Docker 29.8.2，Buildx 0.37.2，Compose 5.5.0，staticcheck 2026.2.2 (0.8.2)，govulncheck 1.9.0。未运行远端 CI，未声称验证 CI 固定的 Chrome156/Node24.14。

OCI SHA-256：`7dedf6120eee7489a88d16af4ab7624efa13e4a4e6bc372648bb4c83880aabd0`。amd64 manifest 为 `sha256:605f354fce514223e73164fe9a53d1f90a837097db1f26db83a3a1b8a181c461`，arm64 为 `sha256:034271d91a06ad1d943bcf21b61f414e6d0df7938aadfdaf52de7baf07d8f820`。仅本地构建，不是 registry 或生产运行摘要。

### 15.4 历史失败、重跑与证据完整性

- 0.10.1 首次 UI gate 与 UI 并行，26 通过/1 失败/0 跳过，退出 1；隔离原样重跑 27/0/0、退出 0。第 13 节及原日志均保留。
- 0.10.2 目录夹具最初缺登录后的 CAPABILITY，7 叶子通过/10 失败、退出 1；修正夹具后最终 18/0/0、退出 0。没有删断言，见第 14 节和原结果 JSON。
- Gmail 本轮较早源码全包 race 于 20:35:30 启动，618/1/0、退出 1；失败为 `TestSyncResumesAfter700WithRealProtocolAndSQLite` 等待超时。20:42:11 隔离重跑 619/0/0、退出 0；随后增加网络代际修复后，20:49:05 最终源码重跑 628/0/0、退出 0。首次日志未覆盖；资源竞争仅为可能原因，不当成已证实根因。本节 21:11:59 的完整重跑再次覆盖固定候选。
- 本轮 Outlook prepare 首次因服务商未标注 archive role 安全停止，退出 1，未执行邮件写入。官方网页可见“存档”，IMAP LIST 也返回同一精确目录；采用现有管理员映射后重做准备、退出 0。保留 `outlook-prepare.json/.log` 和 `outlook-prepare-mapped.json/.log`，不把失败改记为通过。
- 证据采集还遇到旧表名 `tasks` 不存在、未认领浏览器标签、表单精确 label 未匹配，以及 Outlook 网页 TrustedTypePolicy 错误。前几项调整采集方式，无产品断言变更；网页错误仍列为真实对照阻塞。原独立材料的六项 SHA-256 见最终 JSON，提交前/交接时再次一致。

### 15.5 本机 QA 及真实账号分栏结果

21:10 发现 QA 后端 `127.0.0.1:18090` 已停止，HTTPS 代理仍在。21:17:49 在仓库外 0700 私有目录做一致 SQLite、配套 master.key 和旧二进制备份，然后用本轮构建恢复同一启动参数；readyz 返回 0.10.2/schema15。没有关闭 TLS 校验、改变网络策略、开启自动同步或访问远端部署。此为隔离 QA 恢复，不是生产部署。

| 能力 | G-PERSONAL-01 | MS-PERSONAL-01 |
| --- | --- | --- |
| 应用、scope、回调 | 现有 Google 应用已保存；控制台 External/Testing、既有测试用户和 openid/email/mail scope 已核对，无重复注册 | 现有 consumers 应用和加密密钥保留；2028-11-15 到期 |
| 官方账号授权 | 已创建仅四 QA 标签的 pending 配置、打开官方账号选择页；**等待用户本人登录/同意** | 原官方授权保留 connected；本轮重新授权已打开 Microsoft 官方账号选择页，**等待本人选择同一身份** |
| IMAP / refresh / 同步 | **未执行**，没有真实 grant/连接/消息 ID 证据 | 本轮新 Manager 实际取得 refresh grant 后凭据、XOAUTH2、LIST/UIDPLUS/MOVE；首次及重复 `Service.Sync` 均 13 目录/130 条。非修改本地过期时间模拟；未记录原始 token 响应 |
| 分类 / 归档 / Trash / 恢复 / 标记 | **未执行**；受限能力仍拒绝 | 仅七封合成夹具，8 个动作完成；额外一次 Trash/恢复也完成。每项重新同步核对目录和 Seen/Flagged，未知结果 0 |
| 规则 / 保护 | **未执行** | 严格 QA 源 + `[MA-QA-574b047] ` 与 `-02` 主题条件；1 次手动规则 run，完成 2、保护 3、失败/unknown 0 |
| 网页前后对照、人工保留 | **待验** | 网页确认 QA 目录和合成主题；逐动作网页对照被反复出现的 TrustedTypePolicy 加载错误阻塞；人工保留的真实夹具未验。不能用 IMAP 结果代替这些要求 |
| 整体结论 | **真实账号验收待完成** | **部分真实链路通过，整体验收待完成** |

Outlook 先前 19:58 的用户授权已将同步范围改为全部可读取文件夹。本轮开始时为 11 目录/123 条、旧 QA 目录与三封索引均已无，删除原因未确定；没有把历史 126 条当作当前实测。新官方标签确认目标账号后，在专用范围重新创建 `MailAuto-QA` / `MailAuto-QA-Target`，仅通过 IMAP APPEND 加七封带完整 QA 前缀、`.invalid` 地址的合成数据，不使用 SMTP、不外发、不打开私人正文或修改私人邮件。全目录同步继续读取用户已授权的元数据，不能把“私人正文读取 0”误解为“未读取任何私人邮件元数据”。

受控规则 run：**`lpZvlyYYqJVtZPdhmTM6d2r8P2rjYsgR`**；任务 `yVda22OFVtvE59e41R6J6WRq`。扫描 5、完成 2；未读 M-UNREAD-02、旗标 M-STAR-02 和刚变更仍在 180 秒稳定窗口内的 M-FLAGS-02 各保护 1。保持 MaxBatch=20、未读/旗标保护=true、StableSeconds=180、无每日额度，没有为通过减少保护。真实操作 ID 示例：

| 动作 | operation ID |
| --- | --- |
| 分类 M-MOVE-02 | `kNnvETSRZKHrA-05oDdxJbw0NBaUQCe-` |
| 归档 M-ARCHIVE-02 | `yyxvGVhr-3ouqI0whzNjMS7EQjPd-8f4` |
| Trash / 恢复 M-TRASH-02 | `apxUo2OAkU3jfVAYJTpYzWadujJht5Sv` / `ATeBREQYXfBlDZtF5qCWyVq3V89thekv` |
| 标未读 / 已读 M-FLAGS-02 | `pfL6EFsuwfi67QwQ_1Z7RinMMOFcPigp` / `mc2UWrXtzuLkIp_MYNctvRKl4ZZPh5uX` |
| 加 / 取消旗标 M-FLAGS-02 | `wJmbdljJA60RWESoTOg5Ohn2h1Bqc8JQ` / `f4rWqtr3ZSxgXEVERhy5D71jfaQfIbrR` |

本轮总共 12 个真实 operation 全部 completed；其余两个规则 move 和额外 Trash/恢复 ID 也在最终 JSON 中。未制造 unknown、未注入假人工保留记录、未重放未知写入。21:36:41 已停用本轮 QA task/policy，并关闭原邮箱写入；保留合成夹具及记录待主代理核验，不清理邮件证据。全目录范围、自动同步关闭和网络保持原样。真实脚本逐阶段退出码、helper 起止时间及脱敏记录见最终 JSON 和 E 下 `outlook-*.json/.log`；时间明确为 helper 启动后采集，不伪称另有精确编译/启动器时间。临时准备/扫描脚本不入 Git 或镜像。

最后另在真实管理页点击 Outlook 卡片“同步邮件”，页面确认 **130 封、13/13 文件夹完成**。此前 helper 直接调用 `Service.Sync` 的结果不更新 UI 持久 sync_job；此次实际 UI StartSync 另行核对，证据 `E/final-live-state.json`。没有点击全账号同步来使待授权 Gmail 进入失败。

### 15.6 仍待完成、升级与后续责任

1. **Gmail**：本人在已打开的 Google 官方页选择约定测试账号并同意。一次性请求十分钟过期；若失效，从同一本机管理会话的该 Gmail 卡片重新发起。之后仅专用 QA 标签/主题验真实回调、XOAUTH2/LIST/X-GM-EXT-1、字符串 X-GM-MSGID、首次/重复/增量同步、重启后真实 refresh、同身份重新授权、多标签保留无关标签、指定合成 INBOX 邮件归档、标记及受控规则/保护；不全选 INBOX。
2. **Outlook**：本人在已打开的 Microsoft 官方“选择帐户”页选择原已绑定账号完成重新授权；之后再次真实诊断。恢复官方网页正常加载后，仅按 QA 主题补逐动作前后对照；人工保留保护须安全真实夹具，不能伪造待核验操作或直接写保护表代替。
3. **组织账号**：Workspace / M365 继续未验证。Gmail 的安全拒绝边界保持不变，不能借真实验收擅自扩大支持范围。
4. **升级/回退**：0.10.1→0.10.2 无新迁移；仍保留配套数据库/主密钥/私有应用文件。导入文件只只读挂载到运行容器，UID 65532、0400/0600、父目录受限，新 origin 先登记精确回调；不内置 secret。回退需停服务，移除旧程序不认识的部署文件参数；只回到包含 GO-IA-01～04 修复的候选。跨 schema14 回退仍恢复配套快照，不手改 user_version、不自动重放远端动作。
5. **主代理复验**：读取固定源码 SHA/tree、33 文件清单、最终测试 JSON 和原始证据，独立复跑关键测试，核对两家真实缺项。源码提交后的材料提交不扩大代码验收范围。只有主代理独立代码及真实账号验收均通过后，由主代理执行发布流程。

**代码实现、自测及本地候选固定已完成；两家个人账号真实整体验收尚未完成。等待主代理独立验收；尚未发布部署。**

### 15.8 Gmail 真实多标签语义修复与 0.10.3 增量

Gmail 官方回调已于 22:14:22(+08) 成功，G-PERSONAL-01 为 connected/generation2；保存 scope 含完整 `https://mail.google.com/`，refresh token 加密保存。新 Manager 实际 refresh、XOAUTH2、X-GM-EXT-1、四个 QA 标签 LIST 和同步均通过。首次/重复/增量同步分别为 13/6、13/6、14/7（标签条目/唯一 Gmail ID），X-GM-MSGID 均以字符串保存。

第一条真实普通标签移动 `G-MULTI` operation `DgF8upu533oF1a1Dx6UC-ryD1_BWQoy9` 和目标原本已有标签的 `G-EXISTING` operation `SFIfWNVqhfFe14-BIv3whu2QuPhzv2S0` 均进入 `needs_verification`；目标 UID 已保存但源、目标及无关标签仍存在。既有只读核验保留待核验，未取消、清除或盲目重放。真实只读 `X-GM-LABELS` 证据也显示当前选中标签可能不出现在返回集合中，不能单凭该字段判断成员。

普通标签分类路径已修为：先按账号 + X-GM-MSGID 在目标标签视图定位，核对目标 UIDVALIDITY 和可写状态，再使用目标视图 UID 定向 `-X-GM-LABELS.SILENT` 移除源标签，并重新核对源/目标成员。新增夹具覆盖目标已有、目标只读、目标 UIDVALIDITY 变化和移除断线；修正后 IMAP 回归通过。独立新邮件 `G-INCREMENT` 的直接 QA 操作返回目标 UID，网页刷新后只出现在目标标签，证明协议修复生效，但不替代待核验 operation 的结案。

Gmail `G-FLAGS` 的未读、已读、加旗标、取消旗标四项均 completed；网页确认 G-STAR 保持星标、G-UNREAD 保持未读、G-FLAGS 已读且无星标。Outlook 新增未读/已读以及 Trash/恢复四项 completed；恢复后网页再次触发 TrustedTypePolicy 加载错误，未伪称网页恢复后态通过。QA 任务/策略及写入均关闭。

源码候选增量版本为 `0.10.3`、schema15。完整门禁退出 0：race 639/0/0、independent 4/0/0、UI 1032/0/0（18 组合，1280/375/390，页面错误0）、UI gate 27/0/0、release 46/0/0，vet/staticcheck/govulncheck/Compose/构建/amd64/arm64/双架构 OCI 均通过。证据 `.planning/oauth-consent-followup/gates/`；真实和修复证据 `.planning/oauth-consent-followup/`。未提交、未推送、未发布、未部署，原失败和待核验记录保留；等待主代理独立验收。

### 15.7 本人授权返回后的权限核查（2026-11-16 晚间补充）

收到用户“两个账号已授权”及 Gmail 缺少 IMAP 权限截图后，重新只读核对真实绑定状态、官方请求、回调结果和 Google 已关联应用权限，没有将口头确认替代成功绑定：

- Gmail 仍为 pending、generation=1、无主体和 refresh。21:59:07 的浏览器历史结果为 `oauth_scope_missing`；原始 token 响应没有保存，因此无法追溯该次返回 scope 的精确字符串。当前官方请求包含 `openid email https://mail.google.com/`、S256、offline 和精确本机回调。Google 同一目标账号的“Mail Auto Personal QA”权限详情仅列 Google 登录/个人资料，没有 Gmail 数据访问权。这支持“仅登录权限、邮件权限未授予”的判断，未发现可复现的 scope 规范化误判，未放宽服务端校验，也没有改 OAuth 应用。
- 新增正式测试 `internal/mailoauth/granted_scope_test.go`：四种无 IMAP scope 的 code grant 必须在身份验证前拒绝，不泄露 token、不绑定账号；四种包含正确 Gmail scope 的返回，包括 identity scope 的规范化别名和空白分隔，均接受。`go test -race -count=1 -json ./internal/mailoauth ./internal/admin` 退出 0，150 个叶子测试通过（新增 8）、失败 0、跳过 0；相关 `go vet`、新增测试格式与差异检查通过。完整证据位于 `.planning/oauth-consent-followup/`，这批新增测试尚未包含在固定源码提交 `811ab7a` 中；产品实现、版本 0.10.2 和 schema15 未改变。
- Outlook 仍 connected、generation=4，最近一条成功授权审计为 19:34:39；尚无本轮晚间同身份重新授权回调成功证据。22:06:34–22:06:37，新 Manager 无 access-token 缓存的真实只读诊断再次通过 refresh、XOAUTH2 和 13 目录列表，MOVE/UIDPLUS 可用，退出 0；没有改过期时间。
- Outlook 官方网页首次仍发生 TrustedTypePolicy default 冲突；点击官方建议的 Refresh 后恢复，确认是同一绑定邮箱。仅进入专用 QA 目录核对，源 3、目标 3、存档 1。随后仅对 M-FLAGS-02 执行 `mark_unread` / `mark_read`，以网页前后状态补验；operation ID、时间、退出码与截图见 `outlook-web-*.json` 和补充汇总。这是新增两个操作，不能替代其他动作尚缺的逐动作网页对照。每次 helper 完成后恢复 `writes_enabled=false`，任务和策略未开启。

证据汇总为 `.planning/oauth-consent-followup/followup-results.json`，权限和 QA 截图只留在受限本地目录，不入 Git。首次打开 Outlook 默认收件箱时，网页自动渲染了私人邮件列表预览，随后立即进入 QA 目录；没有打开私人邮件或保存这些预览内容，不能将其描述成“未见任何私人正文片段”。没有修改私人邮件、发信、永久删除或访问生产。

剩余最少本人步骤：在本机 Gmail 卡片发起的有效官方授权中选择约定账号，并在权限页明确勾选 Gmail 邮件访问项后继续；只同意 Google 登录不足以完成邮箱授权。请求过期时从原管理会话重新发起。随后才能执行 Gmail 的全部真实 IMAP、刷新、多标签、整理及保护验收。Outlook 的晚间同身份重新授权、其他动作逐项网页前后对照和安全真实人工保留夹具仍待补齐。组织账号仍未验证；等待主代理独立验收；尚未发布部署。

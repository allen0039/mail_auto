# Gmail / Outlook 实施交接

交接日期：2026-11-16。目标：0.10.0 / schema 15。

**等待主代理独立验收；尚未发布部署。** 本报告是实施协作者自测交接，不是独立验收结论。模拟测试完成，真实账号验收待完成；当前不满足生产发布验收条件。

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
| MS-PERSONAL-01 | 拟用个人 Outlook，尚未登录 | 全部未提供 | 0 | 无 | 真实账号验收待完成 |
| Workspace / M365 | 无组织测试账号 | 未提供 | 0 | 无 | 未验证兼容扩展 |

两个个人账号的官方登录/回调、IMAP诊断、目录、首次/后续同步、真实refresh、重启、重授权、专用邮件整理、规则任务和Gmail多标签网页对照均待完成。没有可提供的真实前后状态或 run/operation ID；模拟测试生成的 ID 没有冒充真实证据。

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

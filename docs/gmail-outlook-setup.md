# Gmail / Outlook OAuth 接入与运维

2026-11-16，0.10.0 / schema 15 开发候选。**真实个人 Gmail / Outlook 账号验收待完成；等待主代理独立验收；尚未发布部署。** 本文描述候选实现，不表示服务商线上行为已经实测。

## 范围与能力

个人 Gmail、Outlook.com / Hotmail / Live 使用官方登录。QQ、网易、iCloud 保留授权码/外部 secret。Workspace / Microsoft 365 是未验证兼容扩展，不能推广个人账号结论。共享邮箱、代理访问、服务账号、应用级全租户访问、主权云和本地 Exchange 不支持。

| 操作 | Gmail | 个人 Outlook |
| --- | --- | --- |
| OAuth / IMAPS XOAUTH2 / 刷新 | 已实现，模拟通过，真实待验收 | 已实现，模拟通过，真实待验收 |
| 目录/首次/增量同步及只读正文 | 获取字符串 X-GM-MSGID，保留标签成员条目 | 常规 IMAP UID / UIDVALIDITY |
| 分类 | 添加目标普通标签、移除指定源标签，保留无关标签 | 原生 MOVE 或 UIDPLUS COPY + 定向 UID EXPUNGE |
| 归档 | 仅 INBOX → SPECIAL-USE All Mail，移除 `\Inbox` | SPECIAL-USE / 管理员映射的归档目录 |
| Trash / 恢复 | **未验证，服务端拒绝** | 现有受控移动和恢复，真实待验收 |
| 标记 | 仅 `\Seen` / `\Flagged`，索引跨标签传播 | 现有标记逻辑 |
| 未知写入 | 仅按稳定 ID 只读核验源/目标成员，不重放标签写入 | 现有映射、只读核验和完整保护复查的定向清理 |
| 人工“保留两份” | **拒绝：标签视图不证明独立副本** | 既有完整原文核验与持久保护 |
| 移动/归档撤销 | **拒绝：没有操作前完整标签证据，反向 move 可能移除原有标签** | 既有受控反向移动计划，检查当前位置 |

Gmail 规则按优先级升序、规则 ID 升序，再按文件夹名称和 ref 固定顺序选择源成员。首个匹配、未知或保护结果结束决策，不合并矛盾动作。同运行以账号 + Gmail ID 领取一次，并阻止多个标签上的并发写入；新任务/运行仍可处理。其他已知成员的未读/星标保护阻止全部整理，实际写入前重读远端状态；既有人工保护不被绕过。

UID 仍负责协议定位；X-GM-MSGID 必须是十进制字符串，不能转换为 JavaScript Number。列表、同步进度按文件夹条目计数，Gmail 卡片另列唯一邮件数。特殊目录保留 LIST 原始名称，不硬编码英文 Trash。Gmail 分支不执行 COPY、MOVE、`\Deleted` 或任何 EXPUNGE。

排队、运行、待核验操作从持久冻结计划按位置读取账号和 Gmail ID；自动化领取从持久批次读取同一身份，不依赖当前 messages 索引。同步删除来源、缩小范围、UIDVALIDITY 变化和重启均不释放未结案操作的排他保护。结案后释放并发保护；同一任务运行内另有持久运行领取，不阻止未来新运行的有意处理。未知结果不能通过停止操作清除或重放。

## 固定 origin、回调与网络

先准备用户浏览器可达的固定 HTTPS origin。在“邮箱连接 → 邮箱授权应用”填写 origin，精确回调分别为：

- `https://mail.example.com/api/mail-oauth/google/callback`
- `https://mail.example.com/api/mail-oauth/microsoft/callback`

origin 不带路径、查询或 fragment，回调不从 Host / X-Forwarded-Host 生成。服务商进行浏览器重定向，不要求其服务器主动访问回调。反向代理须转发路径和 Cookie，日志仅记录路径/状态，避免记录 code/state 查询。回调立即 303 跳转到无授权码/state 的页面，并设置 no-store/no-referrer。

管理会话保持 Strict Cookie；发起时另设 Secure、HttpOnly、Lax、10 分钟且仅 `/api/mail-oauth/` 可用的会话 Cookie，支持官方页顶层跳转绑定原会话。会话/管理员版本或 state 失效即拒绝。邮箱 OAuth 独立于 MCP。

浏览器登录用浏览器网络；服务端 token、JWKS 和 IMAP 使用账号选择的出口。代理失败不自动直连；独立网络诊断使用明确选择的诊断出口换 token 和建连。HTTP 禁止重定向，验证 TLS，超时 25 秒，响应上限 1 MiB。前端不能指定任意 OAuth/JWKS endpoint。

| 服务商 | 固定官方网络依赖 | IMAPS |
| --- | --- | --- |
| Google | `accounts.google.com`、`oauth2.googleapis.com`、`www.googleapis.com/oauth2/v3/certs` | `imap.gmail.com:993` |
| Microsoft | `login.microsoftonline.com/{consumers或common}/oauth2/v2.0/authorize`、`token` 与 `discovery/v2.0/keys` | `outlook.office365.com:993` |

## Google 注册

1. 在 Google Cloud / Google Auth Platform 创建本人项目及 Web application OAuth 客户端，设置 Audience、Data Access、联系人和测试用户。
2. 在 Authorized redirect URIs 录入管理界面展示的精确 HTTPS 回调；只在受保护界面填写 client ID 和 client secret。
3. IMAP scope 为 `https://mail.google.com/`，另请求 `openid email` 进行身份绑定。不申请 SMTP、联系人、日历等权限。该全邮件范围比本产品能力更广；产品本身不提供发信和永久删除。
4. External + Testing 涉及邮件权限时，refresh token 通常七天到期，测试用户须包含本人。Production 不代表自动通过验证或永久有效。公开分发 restricted scopes 可能涉及验证和安全评估；个人自用、内部应用请按官方政策确认适用条件。
5. 添加 Gmail 并完成官方登录/同意。验证签名、issuer、audience、expiry、nonce，绑定稳定 sub 与经验证的 email；重授权不能换为另一人的邮箱。自定义域名需手选 Google，Workspace 未验证。

## Microsoft 注册

1. 在 Microsoft Entra 应用注册选择覆盖个人账号的 Supported account types。仅个人账号对应 `consumers`；任何组织目录及个人账号对应 `common`。不接受任意 authority/主权云地址。
2. 添加 Web 平台并录入精确 HTTPS 回调。创建有期限的 client secret，填写 **secret 的值而非 secret ID**；可在界面记录到期日期。不要把密钥发到聊天中。
3. 使用 Office 365 Exchange Online 委托范围 `https://outlook.office.com/IMAP.AccessAsUser.All`，以及 `offline_access openid email profile`。Graph Mail.Read / Mail.ReadWrite token 不能用于 IMAP，不申请应用权限、SMTP 或全租户权限。
4. 个人路径使用本人 Outlook.com / Hotmail / Live，确认允许 IMAP。组织路径还受管理员同意、用户分配、条件访问、Exchange IMAP 开关等限制；未组织实测，不能保证所有租户可用。
5. 验证 ID token 签名、iss/aud/exp/nbf/nonce/tid，以 oid（若有）或 sub 加租户绑定稳定主体。email / preferred_username 仅辅助初次本人邮箱匹配。个人缺少地址声明时必须实际 IMAP 只读登录成功；组织缺少地址声明时拒绝初始绑定。不支持替换 IMAP user 访问共享邮箱。

## 生命周期与错误

添加配置后先待授权，不启动同步，不占已连接的十个名额；待授权暂存最多十个，可从卡片移除。回调先作只读 IMAP/LIST 诊断再提交：Microsoft 诊断失败不绑定；Google 网络失败保存已验证授权并展示网络失败，供稍后重连。

卡片区分未配置应用、待授权、连接/同步、网络失败与需重授权。“断开本地授权”清除 refresh token/state/cache 并停止工作，不代表服务商撤销全部同意；供应商侧撤销请在 Google Account 第三方访问或 Microsoft 应用权限页面完成。

client ID、origin 或账号类型变化会清空旧 refresh token 并要求重授权。相同身份配置更新 secret 保留 refresh token，同时增加配置/凭据版本，使旧 state/cache/连接失效；获取 token 时检测密钥是否可用。Microsoft 缺 oid 时使用客户端绑定的 sub，改 client ID 如导致主体变化，须明确移除重加，不能用地址替换稳定主体。

`invalid_grant`、scope 不足或明确 IMAP 权限拒绝进入需重授权；临时网络、429/5xx 缓存失败至少一分钟，后台仍有界退避。刷新按账号合并，轮换按代际、配置版本和旧密文 CAS，不覆盖重授权/删除；Google 无新 refresh token 保留旧值。access token 仅短期存内存。

## 升级、备份与回退

本候选不得直接进入生产。独立验收及两个个人真实账号验证完成后才发布。

新冻结计划的账号摘要带 `v2:` 前缀，绑定五项旧身份、有效网络路由、OAuth 代际和账号网络策略。已有 schema15 的无版本七字段摘要仅作只读兼容；schema14 五字段摘要仅在旧密码服务商、身份和文件夹完全匹配、当前有效直连时允许只读核验/本地人工结案。它没有历史网络证据，不能证明升级前使用什么出口，因此不允许恢复清理或继续写入；历史代理账号保持待核验，不自动改写摘要，可在官方邮箱人工核对。不会为适配历史摘要自动切换网络。

升级前未执行的旧计划须重新预览；历史排队操作启动时安全失败，未发送写入。已发出且结果未知的记录保持待核验，只读证据充分时结案；需要 COPY 源清理时返回 `recovery_legacy_read_only`，不补发 STORE/EXPUNGE。可用“人工核验保留两份”处理符合完整原文证据条件的旧直连账号；本地确认再次比对事务内完整网络/OAuth 快照，网络期间不持有事务。Gmail 仍不支持“保留两份”。回退原未发布候选二进制也会缺少新保护，不应混用新旧 worker；跨 schema 回退继续按配套备份恢复。

1. 升级前暂停整理写入，记录现有任务、每批数量、未读/星标/人工保护及网络配置。旧二进制执行 `mail-auto backup /安全目录/before-0.10.0.db` 生成一致快照，另备份配套 `master.key`、外部 secret 和私有 Compose，文件 0600/目录 0700。不要只复制活动 SQLite 主文件。详见 [运维文档](operations.md)。
2. 在隔离目录恢复快照与同一主密钥测试启动。14→15 在单一事务新增独立 OAuth 表、字符串 Gmail 身份列/索引，不改 accounts 结构、`account:<id>` 密文上下文、外部文件名、任务和人工保护。重复启动幂等，失败原子回滚。
3. 升级后核对 `/readyz` 为 0.10.0/schema 15、旧账号连通与目录同步及专用任务，再恢复原有写入。不要重建主密钥或手工降低 user_version。
4. 回退须停新服务，恢复升级前数据库、配套主密钥和旧镜像；单换旧二进制会被 schema 拒绝。保留新库取证副本，防止旧快照重放已执行任务。新增授权/索引/操作记录会丢失，供应商侧动作不会自动撤回，须先人工核对再启用写入。

## 真实账号验证的最少外部步骤

用户在受保护页面完成应用配置和官方登录，准备专用数据，不在聊天里提供凭据：

1. 账号代号 `G-PERSONAL-01`、`MS-PERSONAL-01`；准备 MailAuto-QA / MailAuto-QA-Target 专用标签/目录和明确选中的测试邮件。Gmail 为同一测试邮件添加两个测试标签，不使用私人邮件。
2. 每账号验证官方授权、实际 IMAP、文件夹、首次/后续同步；完成真实服务商 refresh grant 并记录成功时间与后续连接。只改本地过期时间不算刷新成功。
3. 重启隔离实例并重新授权，核对稳定主体。分类、归档、标记及 Outlook Trash/恢复后，与服务商网页对比。Gmail Trash/恢复按拒绝矩阵验收；是否补齐由主代理/用户决定，属于交接待决项。
4. 执行受控固定规则任务，记录脱敏 run/operation ID、前后目录/标签和保护跳过；Gmail 多标签只整理一次，无关标签保留。
5. 检查日志、API、审计、截图和交接报告没有 token/secret/code/verifier/私人正文。未完成真实链路前维持“真实账号验收待完成”，不得发布部署或清理生产资源。

## 官方依据（2026-11-16 重新获取）

- [Gmail XOAUTH2 和 scope](https://developers.google.com/workspace/gmail/imap/xoauth2-protocol)
- [Gmail IMAP 扩展、SPECIAL-USE、稳定 ID 和标签](https://developers.google.com/workspace/gmail/imap/imap-extensions)
- [Google OAuth 生命周期和 Testing 限制](https://developers.google.com/identity/protocols/oauth2)
- [Microsoft 个人/组织 IMAP OAuth](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)

协议文档不能替代本项目真实账号写入证据。

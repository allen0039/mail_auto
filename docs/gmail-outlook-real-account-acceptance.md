# 个人 Gmail / Outlook 真实账号验收准备与记录

2026-11-17。原始返工候选 `574b04742ef4b783cb7333de97d2f282ea722bcd` 的四项代码缺陷已独立复验，见 [复验报告](gmail-outlook-independent-reacceptance.md)。0.10.3源码候选 `8f8ee807eb457f065be912d4052096a774901a5a` 的真实记录保留；当前0.10.4/schema15为检查点B本地UX候选，未启动真实QA。最新真实状态以第8.6.3节为准，B候选及门禁另见实施交接第16节。

最新事实：缺IMAP scope是已恢复的历史授权问题；主代理在G-MULTI、G-EXISTING两个已读样本的单封官方详情确认SVC2主题，列表旧标题疑点已证实为Gmail会话聚合，不是SVC2不存在，不代表六封网页全验过。独立只读IMAP已核对六封SVC2身份及成员与索引/冻结计划一致。三条Gmail未知分类写入源目标仍并存，保持 `needs_verification`，未重放、取消或删除；此前两条SVC2 Service move及规则run成功证据保留。Outlook同身份重授权、网页故障后动作对照及真实人工保留等缺项仍阻塞。**真实账号整体验收仍未完成，不发布部署。** 以下历史时点的失败和疑点原文保留，不代表最新结论。

19:30 更新：用户已确认在 MS-PERSONAL-01 准备测试数据，但真实 IMAP 暂未列出 QA 目录；不是已证实的“用户准备了另一个邮箱”问题。仅本机 QA 运行版本现为 **0.10.2/schema15**，包含本轮目录诊断修复和之前未独立验收的应用复用改动。它不是已通过验收的 574b047 固定代码，也未发布到远端。

19:43 更新：通过 Chrome 确认目标账号的服务端目录树同样没有 QA 目录。在其根目录创建 `MailAuto-QA` 后，空目录同步立即成功；随后仅通过 IMAP APPEND 加入三封纯合成测试邮件，首次及重复手动同步均为 **3 封、1/1 文件夹完成**。数据库11项核对通过，未读/旗标保留，无重复索引。没有外发邮件、打开或修改私人邮件，没有执行整理操作。此前失败证据保持原样，详见下文“QA 目录补齐后真实同步通过”。

19:58 更新：用户随后明确要求本机 Outlook 改为全部可读取文件夹并手动同步；实际 12/12 文件夹、126 封，收件箱索引55封，`Deleted`可筛选但为空（0封）；空目录不证明非空回收站展示或整理动作。自动同步继续关闭。此次读取元数据的范围已从 QA 扩大到该账号所有可读取目录；此前的纯 QA 记录仅描述当时的范围。证据 `.planning/outlook-folder-sync/all-folders-sync.json`。Gmail 未添加账号，后续 Gmail 仍须只配置专用测试范围。

## 1. 已准备的隔离环境

| 项目 | 当前状态 |
| --- | --- |
| 管理入口 / 固定 origin | `https://localhost:18444`，仅本机可达，不需要公网域名或隧道 |
| Google 精确回调 | `https://localhost:18444/api/mail-oauth/google/callback` |
| Microsoft 精确回调 | `https://localhost:18444/api/mail-oauth/microsoft/callback` |
| 程序监听 | `127.0.0.1:18090`；HTTPS 代理只发布 `127.0.0.1:18444` |
| QA 程序与私有数据 | `/Users/allen/Library/Application Support/MailAuto-QA/gmail-outlook-574b047/`，目录0700；数据库、主密钥、凭据及未来邮件均在仓库外 |
| HTTPS | 本任务专用 Caddy CA；带显式 CA 校验的 `/readyz` 已返回 ready / 0.10.0 / schema15，未跳过 TLS 校验；浏览器信任仍待用户完成 |
| 安全初值 | 新数据库0账号/0应用；自动同步关闭、MCP关闭，没有执行型任务；未操作其他项目或生产配置 |
| 日志 | HTTPS 代理无访问日志，默认错误日志丢弃，避免回调 code/state 进入日志；程序不启用 OAuth/IMAP 协议调试 |
| 出口检查 | 使用本项目 `network.Route{Key:"direct"}`，7个未认证 HTTPS/IMAPS 端点均连接成功；这不是 OAuth 或邮箱登录成功证据 |

Google token endpoint 的未认证 GET 返回404，仅表示 HTTP/TLS 可达，不表示交换令牌成功。Google OIDC、JWKS、Microsoft consumers OIDC/JWKS 返回200，两服务商993端口 TLS证书校验通过。本次未修改系统代理或任何既有账号网络策略。

本地脱敏证据：`.planning/gmail-outlook-real-acceptance/environment.json`、`network-direct.json`、`official-documents.json`。没有真实邮箱地址、令牌或私人邮件。该目录不得提交到Git；私有QA目录不得打包进镜像。

## 2. 用户先完成本机管理页初始化

在本机终端执行以下命令，会打开一个终端窗口，导入本任务CA到当前用户登录钥匙串、仅在该窗口显示临时管理凭据，然后打开管理页：

```sh
open '/Users/allen/Downloads/Agent_Worker/mail_auto/.planning/gmail-outlook-real-acceptance/initialize-local-ui.command'
```

证书信任会使当前用户的浏览器接受本任务CA签发的证书。CA私钥仅保存在本机受限QA目录；验收结束可从钥匙串撤销此CA。脚本尚未由代理执行，不会自动更改你的证书信任。macOS若要求确认，请在系统对话框完成；不要把系统密码发送到聊天。

首次登录 Mail Auto 后，设置你自己的管理员用户名/密码。若Chrome此前已打开证书警告页，信任后重新加载或重开该页，确保没有证书警告。不要以忽略证书错误的浏览器参数替代信任。

随后进入「邮箱连接 → 邮箱授权应用」。两个服务商的 origin 均填 `https://localhost:18444`，必须与浏览器当前地址一致。此轮先保存应用配置和准备邮件，**暂不点击邮箱官方授权，不保存执行型规则或开启原邮箱写入**。

## 3. Google 应用注册

1. 打开 [Google Cloud Console](https://console.cloud.google.com/)，用你管理应用的Google账号登录。新建独立测试项目，例如 `Mail Auto Personal QA`，不要更改其他已用应用。
2. 进入 Google Auth Platform，设置 Branding（应用名称、支持邮箱、开发者联系邮箱）。Audience 选 External，保持 Testing；在测试用户中加入将接受验证的个人Gmail账号。注册者和测试邮箱可以不同，但测试邮箱须由你本人控制。
3. Data Access 只配置身份范围 `openid`、`email` 和邮件范围 `https://mail.google.com/`。邮件scope较宽，包含超出本产品操作范围的权限；产品仍不提供发信或永久删除，不额外申请SMTP/联系人/日历权限。IMAP方案不需要改为Gmail REST接口。
4. Clients → Create client，类型必须是 **Web application**。Authorized redirect URIs 精确添加：
   `https://localhost:18444/api/mail-oauth/google/callback`
   本方案是服务端Code流程，无需为此填写JavaScript origin，不选Desktop客户端。
5. 在 Mail Auto 的 Google 应用设置填写 Client ID、Client secret 和上述 origin，保存后核对页面显示的回调完全一致。secret只在此受保护表单录入；不发到聊天、不截图、不把下载的客户端JSON放入仓库。暂不添加/授权邮箱。

Google官方允许开发期间使用localhost回调；我们保留HTTPS，以满足当前产品Secure Cookie要求。External + Testing 且含邮件scope时，refresh token通常7天失效；这是本次测试已知限制，不应误报为持续授权永久可用。遇到应用访问/验证限制时记录官方提示，不通过更换scope或绕过组织策略隐瞒阻塞。

## 4. Microsoft 应用注册与 Outlook 设置

1. 打开 [Microsoft Entra 管理中心](https://entra.microsoft.com/)，进入 App registrations → New registration，建立独立 `Mail Auto Personal QA` 应用。注册应用需要可用Entra目录及相应应用注册权限；官方快速入门列出Azure账号/活动订阅和至少Application Developer等前置条件。**只有Outlook邮箱不保证能创建应用**。若提示没有目录/权限/订阅，记录这一步阻塞，不为验收擅自开通收费服务或把个人测试改为组织邮箱。
2. Supported account types 优先选 **Personal Microsoft accounts only**；如果现有目录仅提供“任何组织目录及个人Microsoft账号”路径，也可选该项，但 Mail Auto 仍设置 **consumers**，仅验收个人账号，不据此宣称M365已验证。不能选仅本组织目录。
3. Authentication → Add a platform → **Web**，精确回调：
   `https://localhost:18444/api/mail-oauth/microsoft/callback`
   不启用implicit grant，不启用public client flow；当前使用Web机密客户端Code + PKCE。
4. API permissions 中为 **Office 365 Exchange Online** 添加 **Delegated permissions → IMAP.AccessAsUser.All**。身份/离线范围为 `openid email profile offline_access`，由程序授权请求使用。不要申请Graph Mail.Read/Mail.ReadWrite、SMTP.Send、应用权限、联系人或日历权限；默认的Graph User.Read若不用于本应用可移除。Graph邮件token不能代替IMAP token。
5. Certificates & secrets → New client secret，选择有限有效期并记录到期日期。在 Mail Auto 的 Microsoft 设置填 Application (client) ID、secret的 **Value（值，不是Secret ID）**、authority `consumers`、origin `https://localhost:18444` 和密钥到期时间；保存后核对回调。
6. 在你本人 [Outlook.com](https://outlook.live.com/mail/) 网页中，设置 → 邮件 → 转发和IMAP（Forwarding and IMAP），打开“允许设备和应用使用IMAP”，保存。官方说明Outlook.com的POP/IMAP默认关闭；不需要开启POP，不使用基本认证或SMTP。这里只做账号设置与邮件准备，Mail Auto OAuth同意稍后进行。

Microsoft对localhost回调有端口匹配特例，但本次仍登记并使用精确同一个URI，不混用localhost/127.0.0.1或不同路径。该Web应用的个人账号支持与注册者是否有组织邮箱是两个问题。

## 5. 专用邮件准备

优先使用专门的个人测试邮箱；如使用现有邮箱，仅准备下表明确标识的非私人测试邮件。可从你控制的其他邮箱发送，正文只写无敏感内容的测试编号；Mail Auto不新增发信能力。

Gmail 创建四个普通标签：`MailAuto-QA-A`、`MailAuto-QA-B`、`MailAuto-QA-Target`、`MailAuto-QA-Other`。Outlook创建 `MailAuto-QA`、`MailAuto-QA-Target` 两个普通文件夹；归档和已删除邮件使用稍后探测出的实际SPECIAL-USE目录，不硬编码英文名称。

统一主题前缀 **`[MA-QA-574b047]`**，用G代表Gmail、M代表Outlook。建议每种服务商各准备六封：

| 主题后缀 | 初始状态 | 用途 |
| --- | --- | --- |
| `G-MOVE-01` / `M-MOVE-01` | 已读、无星标；G在QA-A并带QA-Other，M在QA文件夹 | 分类到QA-Target；G只移除指定源、保留无关标签 |
| `G-ARCHIVE-01` / `M-ARCHIVE-01` | 已读、无星标；G必须仍在INBOX，另带QA-A及QA-Other；M在QA文件夹 | 指定这一封验证归档，G应只移除INBOX并保留标签 |
| `G-MULTI-01` | **同一封邮件**同时加QA-A、QA-B、QA-Other，已读无星标 | 跨标签/多源任务不重复整理，QA-Other保留 |
| `M-TRASH-01` | Outlook QA文件夹，已读无旗标 | 仅该封移回收站、再恢复；Gmail此能力按服务端拒绝验收 |
| `G-UNREAD-01` / `M-UNREAD-01` | QA-A / QA文件夹，保持未读、无星标 | 未读保护应跳过 |
| `G-STAR-01` / `M-STAR-01` | QA-A / QA文件夹，已读、有Gmail星标/Outlook旗标 | 星标保护应跳过 |
| `G-FLAGS-01` / `M-FLAGS-01` | QA-A / QA文件夹，已读无星标 | 明确选中后测试已读/未读/星标标记，并核对网页状态 |

Gmail请用“标签”按钮为同一封邮件加多个标签，不要复制成多个不同邮件，也不要用“移至”误移除归档测试邮件的INBOX。不要全选或批量标记私人邮件。

后续初次同步优先限制到QA标签/文件夹。Gmail归档测试需要定位特定INBOX邮件，若INBOX含私人邮件，须先约定最小读取范围；未经核对不直接扩大同步范围或创建覆盖整个INBOX的执行规则。受控规则必须同时限定QA源和完整测试主题/编号，保留既有未读、星标与人工保留保护，不修改每批数量或每日额度。

## 6. 准备完成后的验收顺序和当前记录

用户只需告知“管理页已打开、Google/Microsoft应用已保存、测试邮件已准备”，或说明哪一步被官方门户阻塞；不要提供明文密钥、token或私人正文。代理先核对非敏感配置状态和回调，再提示本人在官方页面登录并同意。授权需在同一浏览器管理会话发起，不从聊天粘贴state/code。

之后逐项记录：官方授权和IMAP诊断 → QA目录首次/增量同步 → 实际refresh grant及重连 → 服务重启/重新授权 → 专用邮件分类/归档/标记和Outlook回收站/恢复 → 受控规则与保护跳过 → Gmail多标签及网页状态对照。真实refresh必须确实访问服务商token endpoint成功，不能只改本地过期时间。未知写入只核验，不重放。

| 账号代号 | 官方授权/IMAP | 真实刷新/重启/重授权 | 测试邮件操作/规则 | 真实run/operation ID | 当前结论 |
| --- | --- | --- | --- | --- | --- |
| G-PERSONAL-01 | 未开始 | 未开始 | 未开始，写入0 | 无 | 等待应用/用户登录及专用数据 |
| MS-PERSONAL-01 | 已通过：官方授权、IMAP诊断、QA首次/重复同步 | 刷新路径于 2026-11-16 18:53 重启后诊断通过；重新授权未测 | QA合成邮件APPEND 3封，未读/旗标保留；整理动作/规则未测 | 无（同步任务按账号持久化，无整理operation） | 同步阻塞已解除，整体真实验收仍待完成 |

Gmail Trash、恢复、特殊目录移动、“保留两份”、未知标签写入自动补齐及移动/归档撤销继续服务端拒绝；不能把这些记为真实写入通过。Workspace/M365均未验证。真实证据补齐且独立核验前，不发布版本标签或镜像、不部署Oracle1、不清理生产资源。

## 7. 本次重新核对的官方资料

### 2026-11-16 个人 Outlook 官方授权与 IMAP 实测

16:34（Asia/Shanghai）完成 MS-PERSONAL-01 的官方账号同意、回调绑定和独立连接诊断。实际运行验证实例为 **0.10.0/schema15**，不是尚未部署的 0.10.1 应用复用构建。Microsoft 官方同意页正确显示身份、离线访问和邮件读写（不含发送），使用 consumers、Code + PKCE S256、Outlook IMAP scope；没有以 Graph 邮件 token 替代。

用户首次接受后，本地仍是 pending、未保存 refresh；未确定首次回调失败原因。重新发起授权、选择相同本人账号并继续官方应用确认后，状态为 connected、error_code 为空、凭据代际 2，refresh 在数据库中仅为 Vault 密文。未读取或保存 token 明文到证据。客户端配置版本保持 1。

管理页“连接诊断”实测通过：TCP 5 ms、目标 TLS 194 ms、XOAUTH2 登录及 LIST 3049 ms；直连 `outlook.office365.com:993`，无出口回退，读取 11 个文件夹。同步范围固定 `MailAuto-QA`、自动同步关闭；索引 0、任务 0、真实邮件写入 0，没有读取私人正文或执行整理。截图及脱敏摘要为 `.planning/oauth-app-reuse/outlook-imap-success.jpg`、`outlook-authorization-imap.json`。

这一步证明新目录即使 API 选择器没有列出 Exchange Online，个人账号仍能完成官方 Outlook IMAP scope 同意和真实连接；未修改租户权限或添加 Graph 邮件权限。**仅本账号授权及连接通过**，不推广到组织账号，也不等于整体真实验收通过。后续已在重启后完成真实 refresh 路径及 IMAP 连接验证；QA 首次/增量同步、重授权恢复、专用邮件动作和规则保护仍待执行。没有发布或部署。

### 2026-11-16 个人 Outlook 重启后刷新与连接实测

18:53（Asia/Shanghai）在隔离本机 QA 实例中重启 Mail Auto，确认新进程启动后立即运行 Outlook 连接诊断。进程重启清空仅驻留内存的 access-token 缓存；`mailoauth.Manager.Credential` 在缓存缺失或失效时向配置的 Microsoft token endpoint 发送 `grant_type=refresh_token`，随后诊断成功完成 Outlook XOAUTH2 登录和 IMAP LIST。结果：TCP、TLS、XOAUTH2、LIST 均通过，读取 11 个文件夹，直连；自动同步关闭、索引邮件 0、任务 0、真实写入 0。此证据根据实现路径和成功连接确认真实 refresh grant 路径被执行；未留存服务商 token endpoint 审计事件或原始响应，也不声称 Microsoft 本次轮换了 refresh token。refresh token 明文未读取或记录。

脱敏摘要为 `.planning/gmail-outlook-real-acceptance/outlook-refresh-after-restart.json`。现有 Outlook 网页中尚未发现 `MailAuto-QA` 或 `MailAuto-QA-Target`，也没有专用测试邮件，所以没有开始真实邮件同步或任何写入。请在 Outlook 网页创建上述两个文件夹，并在 `MailAuto-QA` 中准备 `M-MOVE-01`（已读、无旗标）和 `M-UNREAD-01`、`M-STAR-01`（分别保持未读、保持旗标）等明确非私人测试邮件；正文只放测试编号。准备好后告诉我“Outlook QA 邮件已准备”，无需发邮件正文或凭据。之后我会把 Mail Auto 文件夹范围限制在这两个 QA 文件夹，执行同步和受控整理，再逐项核对 Outlook 网页前后状态。

### 2026-11-16 QA 文件夹同步受阻

19:04（Asia/Shanghai）用户表示已准备测试数据。隔离实例保持自动同步关闭，手动同步仅配置的 `MailAuto-QA`；结果为 0 封、0/1 文件夹，页面提示打开文件夹时服务器拒绝请求或返回无效响应，未完成文件夹保留原索引。结果不能证明目录不存在。浏览器当前 Outlook 网页所显示的邮箱与隔离实例已授权邮箱不同，且当前网页未看到 QA 文件夹；无法核对测试数据是否位于已授权邮箱。等待用户在 Outlook 网页切换至已授权邮箱，并确认两个 QA 文件夹及专用邮件所在位置后继续。尚未整理或写入任何邮件，未发布部署。

### 2026-11-16 QA 目录真实 IMAP 排查及诊断修复

用户明确确认测试对象是已授权的 MS-PERSONAL-01。19:18 使用现有 Vault 凭据、既有直连网络策略执行官方 IMAPS/XOAUTH2 和只读 LIST/EXAMINE：LIST 返回 11 个目录，无名称包含 MailAuto 的目录；`EXAMINE MailAuto-QA` 返回目录不存在。只保留固定错误分类、目录总数及 QA 路径，未保存原始错误、token 或其他目录名称。证据 `.planning/outlook-folder-sync/live-list-examine.json`。这证明在该时刻此 IMAP 会话无法看到指定 QA 目录；不等于断言用户没有在客户端准备数据，仍需核对客户端目录是否保存在服务器、是否为嵌套目录以及实际完整路径。

本地候选 0.10.2 改进 EXAMINE 拒绝后的只读 LIST 核对，缺失或不可选目录提示 `folder_unavailable` 及完整路径检查方法，成功路径不额外 LIST；目录存在但被拒绝、LIST 自身失败或网络错误不误报缺失。覆盖手动、自动增量、IDLE，未修改写入路径、认证、网络或同步范围。连接诊断新增可展开的完整 IMAP 路径，HTML 转义防止目录名注入。

已先在仓库外私有备份 QA 数据库、主密钥和旧二进制，再仅更新本机 QA。19:27 左右在真实管理页重跑同步：仍 0/1、0 封，新提示明确指出目录不在 IMAP 可读列表；随后连接诊断 TCP/TLS/XOAUTH2/LIST 全部通过，再次列出 11 个目录且无 QA。19:30 带显式 CA 校验的 `/readyz` 为 ready/0.10.2/schema15、退出0，OAuth connected、索引0、operation0、真实写入0。证据 `.planning/outlook-folder-sync/live-candidate-result.json`、`qa-folder-error.png`。不将错误提示修复表述为实际同步/整理通过。

下一步只需本人在该账号 Outlook 网页确认 QA 目录已保存在服务器，并告知实际完整路径；如果网页确实显示目录而 IMAP 仍未列出，再继续排查服务商目录可见性。代理没有擅自创建空目录、读取私人邮件、改成全邮箱同步或反复重新授权。测试汇总见 `docs/outlook-folder-sync-test-results.json`，新增代码仍待独立复验，整体真实验收未完成，尚未发布部署。

### 2026-11-16 QA 目录补齐后真实同步通过

用户授权通过 Chrome 检查并修复直至同步成功。19:37 后检查目标账号的官方 Outlook 网页，邮箱根节点与已授权账号一致，服务端目录树无 `MailAuto-QA`，与之前只读 IMAP LIST 的结果一致。未打开私人邮件，网页自身邮件预览未作为验收证据保存。通过文件夹菜单在根目录创建 `MailAuto-QA`，网页确认目录已保存、0项。没有修改 OAuth 应用、令牌、网络策略、自动同步开关或同步范围。

19:39:08 本机 QA 手动同步为完成、0封、1/1文件夹，证实打开目录失败已消失。随后在目录为空的前提下，仅向该专用目录 IMAP APPEND 三封合成夹具：`M-MOVE-01`（已读、无旗标）、`M-UNREAD-01`（未读）、`M-STAR-01`（已读、有旗标）。发件人/收件人为固定 `.invalid` 测试地址，正文仅测试编号和合成数据说明；没有使用 SMTP、外发邮件或复制私人邮件。临时准备程序校验目标账号、精确路径及空目录前置条件，未加入产品发信功能。

19:41:03 首次非空同步完成，3封、1/1文件夹；19:41:39 重复同步同样完成，3封、1/1文件夹，错误为空、无重复索引。Outlook 网页显示三封对应测试邮件及1封未读，Mail Auto列表对应三条。以只读数据库核对 OAuth connected、任务 completed、允许范围不变、精确三条测试主题、仅QA目录、未读/旗标保留、flags_known、无重复索引和整理operation为0，共11通过/0失败/0跳过。`sync_jobs`按账号更新，不提供单独run ID；此次没有整理operation ID。

脱敏证据：`.planning/outlook-folder-sync/qa-preparation.json`、`live-sync-success.json`、`qa-sync-success.png`。先前的 `live-list-examine.json`、`live-candidate-result.json`及失败截图均保留。此次 QA 数据写入为3封合成APPEND、目录创建1个；整理邮件写入0、外发0、私人邮件打开/修改0。本轮未新增产品代码，仅补齐远端专用测试目录和夹具并复核已有修复；19:43重跑正式目录回归 `go test -race -count=1 -json ./internal/imap -run '^TestSyncFolder'`，退出0、18叶子通过/0失败/0跳过，Go 1.27.4，见 `targeted-live-confirmation.json`及对应JSONL。

Outlook 分类、归档、回收站、恢复、标记动作、受控规则及重新授权仍未完成，后续先准备并明确允许 `MailAuto-QA-Target`，再继续受控测试，不能把同步通过推广为整理通过。Gmail真实验收及组织账号验证仍未完成；0.10.1/0.10.2增量尚待独立复验。**等待主代理独立验收；尚未发布部署。**

### 2026-11-16 应用注册补充记录（早于上述实测）

用户完成 Azure 免费账户注册后，Default Directory 已允许注册应用。已创建 Microsoft `Mail Auto Personal QA`，支持账号类型仅个人 Microsoft 账号、Web 回调 `https://localhost:18444/api/mail-oauth/microsoft/callback`。经用户确认创建 730 天（24 个月）客户端机密，门户到期日为 **2028-11-15**；已直接存入本机 Mail Auto 的 Vault 加密配置，authority 为 consumers。未在报告、截图或源码保存密钥值。

两家实际应用配置已一起导出到仓库外私有目录（父目录 0700、文件 0600），供后续部署只读载入。隔离新实例及其重启均自动配置 Google/Microsoft，版本保持 1，邮箱账号数 0；退出码 0。证据 `.planning/oauth-app-reuse/both-provider-export.json`、`both-provider-reuse-smoke.json` 和关联脱敏日志；私有文件路径仅保留在本地运维证据中，文件内容不入 Git/镜像。此次没有运行发布部署。

权限前置状态：新 Azure 目录的 API 选择器未列出 Office 365 Exchange Online，未添加错误的 Graph 邮件权限。应用默认创建了 Graph User.Read 静态项；Mail Auto 不请求该 scope。后续须在官方个人账号授权页核对动态请求的 `https://outlook.office.com/IMAP.AccessAsUser.All`、offline_access 及身份范围，并记录实际同意结果；如官方返回资源/权限错误，再处理目录配置，不能把客户端配置成功等同于 IMAP 授权成功。

**Google/Microsoft 客户端配置均已保存；Outlook 个人账号官方授权、IMAP、重启后刷新路径和QA首次/重复同步已通过，Gmail真实邮箱验收未开始。** 当前已有3封纯合成QA夹具，整理动作/规则/重新授权仍待验，无整理run/operation ID证据；组织账号仍未验证。未发布部署。

2026-11-16实际获取七份官方页面，curl退出码全部0；原始公开HTML及获取记录保存在本地证据目录。不把协议支持等同于本产品真实测试通过。

- [Google服务端OAuth及localhost回调](https://developers.google.com/identity/protocols/oauth2/web-server)
- [Google refresh token与Testing七天限制](https://developers.google.com/identity/protocols/oauth2)
- [Gmail IMAP XOAUTH2及全邮件scope](https://developers.google.com/workspace/gmail/imap/xoauth2-protocol)
- [Microsoft IMAP OAuth、个人账号及scope](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)
- [Microsoft应用注册前置条件与账号类型](https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-register-app)
- [Microsoft回调URI及localhost规则](https://learn.microsoft.com/en-us/entra/identity-platform/reply-url)
- [Outlook.com开启IMAP与服务器设置](https://support.microsoft.com/en-us/office/pop-imap-and-smtp-settings-for-outlook-com-d088b986-291d-42b8-9564-9c414e2aa040)

## 8. 0.10.2 固定候选的晚间真实验收增量

源码候选 `811ab7a8c72c00c11f6b314841e8506c0bdddb58`，tree `97d459d270bc28a35df8946fb74a247534772fc5`。完整交接见实施交接第 15 节，最终命令、时间、退出码、测试/真实结果、文件 SHA-256 与证据索引见 `docs/gmail-outlook-final-candidate-test-results.json`。本节不是主代理独立复验结论。

### 8.1 环境和同步范围

21:10 核查发现本机 QA 后端已停止，导致管理页应用设置读取失败；HTTPS 代理仍运行。配套私有备份一致 SQLite、master.key 和旧程序后，21:17:49 用本轮构建恢复原启动参数，readyz 为 ready/0.10.2/schema15。自动同步及 MCP 均保持关闭，未改变账号网络、每批数量或保护。只操作本机隔离环境，无 Oracle1 访问、发布部署或生产清理。

Outlook 保留用户 19:58 明确授权的全部可读取文件夹范围 `[]`。开始时索引已变为 123 条/11 目录、旧 QA 数据不在；官方网页也没有旧 QA 目录，删除原因未确定。没有回滚他人数据。重新在目标账号的根目录创建两个专用 QA 目录，额外加入七封带 `[MA-QA-574b047] ` 前缀的纯合成邮件；无 SMTP、外发或私人邮件修改。全目录同步会读取已授权的私人邮件元数据，私人正文读取为 0。

Gmail 仅创建待授权账号，允许范围精确为 `MailAuto-QA-A`、`MailAuto-QA-B`、`MailAuto-QA-Target`、`MailAuto-QA-Other`，没有扩大到 INBOX 或全部邮箱。现有应用、scope、Testing 和测试用户已核对，没有重复注册应用或把任何凭据写进报告。

### 8.2 Outlook 实际执行与证据

| 阶段 / 时间（2026-11-16 +08:00） | 结果 | 本地证据（`.planning/oauth-final-handoff/` 下） |
| --- | --- | --- |
| prepare，21:22:19–21:22:22 | 退出 1：IMAP 返回 MOVE/UIDPLUS、13 目录，但没有 archive role。未执行邮件写入，保留失败 | `outlook-prepare.json/.log` |
| 管理员映射后 prepare，21:23:27–21:24:38 | 退出 0：将官方网页“存档”与 LIST 的精确同名目录对应，保留现有显式映射能力；只向两个空 QA 目录中的源目录 APPEND 七封合成邮件；首次/重复 `Service.Sync` 均 13 目录、130 条 | `outlook-prepare-mapped.json/.log` |
| 分类/归档/Trash/恢复/标记，21:25:38–21:30:15 | 退出 0：8 个 operation completed，逐项真实回读/同步核对目录、未读和旗标 | `outlook-actions.json/.log` |
| 严格范围规则，21:30:57–21:31:42 | 退出 0：扫描 5、完成 2、保护 3、失败/unknown 0；手动触发，不排程 | `outlook-rules.json/.log` |
| 为网页对照额外执行一次 Trash / 恢复 | 两个 operation 均 completed，恢复到 QA-Target；网页加载阻塞，不能记为网页对照通过 | `outlook-web-trash.json/.log`、`outlook-web-restore.json/.log` |
| 关闭 QA 执行，21:36:38–21:36:41 | 退出 0：本轮 task/policy 停用，writes_enabled=false；合成数据和记录保留供复验 | `outlook-close.json/.log` |
| 最后通过管理页点击 Outlook“同步邮件” | 页面实际显示完成 **130 封、13/13 文件夹**；全目录及自动同步关闭不变 | 最终结果 JSON 的账号状态；`final-live-state.json` |

上述时间为 helper 自身记录；编译/启动器精确时间未另采集，不虚构。每阶段实际 `go run` 命令与退出码在最终测试 JSON；CLI 直接调用 `Service.Sync` 不更新管理页持久 sync_job，因此最后另通过真实 UI StartSync 复核，不能把较早卡片的 123 条误记为最新 CLI 结果。

本轮真实 run ID：**`lpZvlyYYqJVtZPdhmTM6d2r8P2rjYsgR`**，task ID `yVda22OFVtvE59e41R6J6WRq`。源仅 `MailAuto-QA`，规则同时要求完整 QA 前缀和 `-02`；完成 M-RULE-02、M-TRASH-02 两封。M-UNREAD-02、M-STAR-02 和处于默认 180 秒稳定窗口的 M-FLAGS-02 各保护 1，保护未减少；MaxBatch=20，未设置每日额度。规则动作 ID 为 `3fgx5_gowWTmfSGxWc5QP-MbbIplSZ98`、`xO90xj622qGTdEwZxPuTO3fqGhNFITmj`。

八个手动动作及额外 Trash/恢复 ID 见交接第 15.5 节和最终 JSON，合计 **12 个 completed operation**；unknown 为 0，没有人为制造未知结果、直接写保护表或重放未知操作。最终状态：MOVE/RULE/TRASH 三封在 QA-Target，ARCHIVE 在已映射存档，FLAGS 已读无旗标、UNREAD 未读、STAR 已读有旗标，后三封仍在 QA 源。

真实 refresh 证据为本轮新建 Manager 无 access-token 缓存时，`Credential` 成功返回令牌后实际 XOAUTH2/LIST 和写入均通过，token endpoint 由既有固定官方实现请求。没有修改本地到期时间来冒充成功；未保存 provider 原始响应，也不声称每次 Microsoft 都轮换 refresh。

### 8.3 尚未通过的真实要求

- **Gmail 本人授权**：Chrome 已打开 Google 官方“选择账号”，需用户本人选择约定测试账号并完成应用同意；十分钟一次性请求过期后，从同一本机管理会话的 Gmail 卡片重新发起。当前仍 pending、无绑定主体或 refresh；没有任何真实 Gmail IMAP、X-GM-MSGID、同步、写入或 run ID。之后全部按专用标签/完整测试主题验收，不操作私人邮件。
- **Outlook 同身份重新授权**：已从该账号卡片发起，Microsoft 官方页停在“选择帐户”，需本人选择原绑定身份完成登录/同意。既有授权仍 connected，不假定重新授权已经通过。
- **逐动作网页对照**：曾在新标签核对账号、QA 目录及合成主题；随后 Outlook 官方页反复报 TrustedTypePolicy default 冲突、应用加载失败。未更改浏览器扩展、系统安全或网络配置来绕过，根因未确定。IMAP completed 与同步结果不能替代每项网页前后对照；网页正常后只按完整 QA 主题补查。
- **人工保留保护**：本轮没有可安全使用的真实人工结案夹具，未伪造待核验或保护记录。正式模拟回归已过，但真实要求仍待验。Gmail 多标签、跨源互斥和保护同样待真正授权后验证。
- **组织账号与受限操作**：Workspace/M365 未验证；Gmail Trash/恢复、特殊目录移动、保留两份、move/archive undo、未知标签写入补齐仍服务端拒绝。

**代码实现和自测已完成；Gmail 与 Outlook 真实整体验收均未完成。等待主代理独立验收；尚未发布部署。** 下一步最少本人动作是完成 Google 和 Microsoft 官方账号选择/同意；不要向聊天提供密码、密钥、验证码或 token。

### 8.5 Gmail 回调成功后的专用 QA 实测及分类阻塞

Gmail 官方回调于 22:14:22(+08) 成功，G-PERSONAL-01 为 connected/generation2，scope 包含完整 `https://mail.google.com/`。新 Manager 实际 refresh/XOAUTH2/X-GM-EXT-1 通过；四个 QA 标签首次/重复/增量同步分别为 13/6、13/6、14/7（标签条目/唯一 Gmail ID），Gmail QA-A 网页显示七封合成主题、未读及星标。

真实分类发现 `G-MULTI` 和目标原本已有的 `G-EXISTING` 均进入待核验，目标 UID 已保存但源标签没有安全移除，源、目标和无关标签仍同时存在。原 operation 与只读核验均保持 `needs_verification`，不取消、不删除、不重放。独立新邮件 `G-INCREMENT` 直接 QA 操作返回目标 UID，网页刷新确认只存在于目标标签；该证据用于验证修复，不替代待核验操作结案。

代码候选增量至 0.10.3：分类先在目标标签视图按稳定 Gmail ID 定位，核对目标 UIDVALIDITY/可写状态，再移除源标签；新增目标已有、只读、UIDVALIDITY 变化和断线夹具。标记动作四项 completed，网页确认 QA 标记状态。Gmail 规则/分类整体验收仍未通过，因未知写入尚未安全结案；Trash/恢复、人工保留及规则 run 未执行。

Outlook 本轮补充未读/已读、Trash/恢复四项 completed；恢复后的官方网页仍有 TrustedTypePolicy 错误，不能算恢复后网页对照通过。完整门禁 race 639/0/0、independent 4/0/0、UI 1032/0/0、UI gate 27/0/0、release 46/0/0，其余检查和双架构构建均退出0。候选已固定为提交 `8f8ee807eb457f065be912d4052096a774901a5a`、tree `424d02806fb88ba53db0f526ebfee0c106bfc229`。证据位于 `.planning/oauth-consent-followup/` 和 `gates/`。**Gmail 与 Outlook 真实整体验收仍未完成；等待主代理独立验收；尚未发布部署。**

### 8.6 Service QA 返工补证（2026-11-16夜至11-17）

本节增量更新，不覆盖8.3–8.5的历史失败。源码仍为 `8f8ee807eb457f065be912d4052096a774901a5a`、0.10.3/schema15，仅材料变更。证据根目录 E=`.planning/service-qa-followup/`，全部门禁、版本和偏差见交接15.9及最终JSON `service_qa_followup`。

23:47:15 `runtime-gate.json` 确认旧PID81359停止，配套数据库/密钥已私有一致备份，单一候选Worker、queued/running=0、unknown=3、写入/任务关闭。只读检查器不是第二个Worker。SVC2合成APPEND早于完整正式gate、晚于readyz，操作入队晚于完整gate；不隐藏准备时序偏差。

| 专用真实操作 | operation / run ID | 结果 |
| --- | --- | --- |
| 历史G-MULTI unknown | `DgF8upu533oF1a1Dx6UC-ryD1_BWQoy9` | needs_verification，其他标签执行busy |
| 历史G-EXISTING unknown | `SFIfWNVqhfFe14-BIv3whu2QuPhzv2S0` | needs_verification，其他标签执行busy |
| 本轮SVC1 G-MULTI | `mzbhsq_R92TMz16la0UKlw_ELQbiolkr` | 新增needs_verification，其他标签执行busy |
| 新身份SVC2 G-MULTI分类 | `Siazgff3sgG4RbiuzRXxkLEqhfv_vkOW` | Service路径completed；源QA-A移除，QA-B/Other及目标保留；撤销安全拒绝 |
| 新身份SVC2 G-EXISTING分类 | `8MnX60D-IlqRRZ-xxGyc4oXNzOT1f6zp` | Service路径completed；原有目标及Other保留；撤销安全拒绝 |
| 多源规则run / operation | `ZqhwkvGwcNlpUFwzCMie1oFPDPcPlGIt` / `NiDo0OV9zjaJly738LVvCuhfDj5klM7u` | completed；扫描28标签条目、领取完成1逻辑邮件、保护汇总7、失败/unknown0 |

三条unknown不是全为新增；最终 `gmail-v2-audit-final.json` 三项alias_busy=true，稳定ID及冻结计划保持，不取消、不清除、不重放。早期audit失败记录保留，最终审计通过不代表结案。规则保护7无逐封原因持久证据，不臆造未读/星标/人工保留明细。阶段后writes_enabled=false，task/policy关闭，最终queued/running=0、三unknown保留；仅四个专用QA标签/合成邮件写入，私人邮件修改0、外发0。

本轮门禁：race639/0/0、Gmail定向18/0/0、原overlay4/0/0、UI1032/0/0（18组合三宽度）、UI gate27/0/0、release46/0/0，其余静态检查/源码格式/Compose/构建/双架构编译/diff退出0。首次runner在race后记录路径异常退出1，原样重跑因已有证据退出1，余下门禁另行执行退出0；不把记录器失败隐藏。全仓格式命令0但空输出断言1，4个既有临时文件未格式化。639为当前候选叶子统计，旧619/628等来自更早候选日志，不累加、不覆盖。本轮未重跑OCI构建，引用15.8既有候选证据。

**未完成**：Gmail网页搜索与SVC2前缀/索引不一致，逐动作官方网页对照未通过；三个unknown未结案、真实人工保留及完整归档/重启和同身份重授权验收未补齐。Outlook同身份重授权、TrustedTypePolicy故障后网页对照和真实人工保留待验；Workspace/M365未验证。请本人仅核对官方网页专用SVC2 QA邮件标签；主代理独立复验原始证据及关键测试，不能以Service/IMAP成功冒充整体验收。**等待主代理独立验收；尚未发布部署。**

#### 8.6.1 主代理网页矛盾与只读身份复核（2026-11-17补充）

主代理只读查看 Gmail `MailAuto-QA-Target` 时看到4封旧 `[MA-QA-811ab7a]` 邮件（G-RULE、G-EXISTING、G-MULTI、G-INCREMENT），未看到SVC2新主题；此前搜索SVC2 RFC Message-ID却返回旧SVC1 G-EXISTING。此为主代理现场反馈，本次未重新操作浏览器，不能归因于缓存、账号错误、搜索语义或Gmail线程合并，矛盾原因未确定。

只读复核原helper源码 `gmail_service_v2.go`：APPEND使用主题 `[MA-QA-8f8ee80-SVC2] ` + suffix，RFC Message-ID为 `<SUFFIX-8f8ee80-SVC2-20261116@mailauto-qa.example.invalid>`；六个suffix为G-MULTI、G-EXISTING、G-RULE、G-UNREAD、G-STAR、G-FLAGS。APPEND后使用Message-ID及Subject双条件定位且要求恰好一个UID。`gmail-v2-prepare.json`记录准备成功和13成员/6逻辑邮件；actions前13/6、后12/6，rules后12/6。原快照只保存suffix、成员、标记及Gmail ID位数，没有逐行完整稳定ID，不能追溯补造原始协议证据。

当前本地SQLite中六个SVC2完整主题及RFC Message-ID与上述模板一致。两条completed move和规则completed操作的冻结计划中，完整主题、RFC Message-ID、X-GM-MSGID与当前索引对应成员一致；稳定ID SHA256前16位分别为G-MULTI `b5a650bd5484f492`、G-EXISTING `aed7e635bb4a3f41`、G-RULE `206b8cf1806954ff`（仅供脱敏交叉核对，不用RFC Message-ID替代Gmail稳定身份）。当前12成员/6稳定ID及标记与原 `gmail-v2-rules.json` after_rules逐项相符；目标索引包含这三封SVC2，三个unknown仍在，queued/running=0。

**证据边界**：源码、历史准备成功报告、当前索引和冻结计划内部相符，支持原报告所用SVC2身份，并不能证明此刻服务商网页或远端成员状态；更不能据此否定主代理看到的旧主题。本次未发起任何新的IMAP请求、APPEND或标签写入，未重放unknown。脱敏复核记录在 `.planning/service-qa-followup/svc2-readonly-identity-crosscheck.json`，原actions/rules证据未修改。官方网页矛盾仍阻塞真实整体验收。

运行状态另有偏差：检查时发现QA后端PID32026仍监听18090，并非已关闭；确认其为本项目私有QA可执行文件后发送SIGTERM，随后无监听，未重启。关闭后普通只读SQLite因WAL侧文件打开失败，改用已停机数据库的 `mode=ro&immutable=1` 完成只读核对，没有创建侧文件或修改业务记录。保持QA后端关闭，不发布部署。

#### 8.6.2 检查点A：停机数据库与身份只读复核（2026-11-17）

按主代理授权执行只读检查：实际 HEAD 为 `891414354e46526dc323a05a1ee126f71c665870`，源码候选仍为 `8f8ee807eb457f065be912d4052096a774901a5a`、0.10.3/schema15。18090 无监听，未启动 Worker；数据库无 WAL sidecar，确认无活动写入后以 `mode=ro&immutable=1` 读取必要列。user_version=15；两个账号 `writes_enabled=0`，两个任务均停用，queued/running=0，completed=24，needs_verification=3。三条 unknown 的状态、冻结计划 SHA256、settings version 和稳定ID脱敏摘要见 `.planning/service-qa-followup/checkpoint-a-readonly.json`。

当前本地索引仍有 SVC2 六个逻辑稳定ID、12个成员条目，与 `gmail-v2-rules.json` after_rules 逐项一致；helper源码确认六个主题后缀及RFC Message-ID模板，冻结完成计划中的G-MULTI、G-EXISTING、G-RULE与索引对应。因后端已停且本检查点禁止启动新认证helper，没有执行新的远端IMAP查询；不能把本地索引当作当前服务商状态。

主代理报告的官方网页现场仍与本地/历史证据矛盾：MailAuto-QA-Target显示4封旧 `[MA-QA-811ab7a]`，精确SVC2搜索曾返回旧SVC1邮件。本轮未导航或交互该主代理所属浏览器会话，未归因于缓存、身份、线程聚合或产品缺陷。历史 `oauth_scope_missing` 只证明当时授权返回缺少邮件scope；本检查点未读取当前OAuth响应、token或secret，不能宣称当前仍缺scope，也不降低 `https://mail.google.com/` 校验。

三个unknown证据足以证明稳定身份和冻结计划存在，但没有本轮提供的服务商只读核验结果；因此均**证据不足以正式结案，继续保留 `needs_verification`**，未调用恢复接口、未手改SQL、未跨标签绕过busy。检查点A到此停止，等待主代理复核后再决定是否进入B/C。

#### 8.6.3 检查点A远端只读补齐与会话聚合澄清（2026-11-17 00:39–00:40）

纠正8.6.2边界：禁止启动后端/Worker不等于禁止独立只读IMAP诊断，8.6.2当时只完成A的本地部分，不能算完整检查点完成。主代理再次明确授权后，新增独立一次性 `remote_readonly.go`，不复用prepare/actions helper，不启动Worker、不调用Service.Sync或任何恢复/执行接口。先在既有私有QA目录完成SQLite及配套主密钥备份，确认schema15；store.Open沿用现有schema及维护路径，没有新增迁移。只允许现有OAuthManager按CAS刷新授权元数据，未输出token、认证报文或原始OAuth响应。

00:39:36–00:40:28 实际执行新helper：新Manager真实refresh grant、既有Network策略下TLS/XOAUTH2成功；账号授权状态前后均connected，保存scope包含官方 `https://mail.google.com/`。先前缺IMAP scope截图是历史失败，不是本次仍存在的阻塞；本次没有放宽scope校验，也没有保存服务商原始scope响应或声称refresh token发生轮换。

仅对QA-A、QA-B、QA-Target、QA-Other执行EXAMINE（检查ReadOnly），逐一用Subject+RFC Message-ID及X-GM-MSGID交叉SEARCH；FETCH仅UID、FLAGS、ENVELOPE、X-GM-MSGID/X-GM-LABELS，不取正文。9个已知合成稳定ID × 4目录共36次成员检查，23个命中成员的完整主题、RFC Message-ID及稳定ID均相符，所有双条件搜索与稳定ID搜索一致；未命中也保留。非QA标签只记录存在性，不输出名称或继续访问。X-GM-LABELS会省略当前选中标签，因此以下成员来自各目录只读SEARCH/FETCH，而非单靠标签集合推测。

SVC2完整主题均为 `[MA-QA-8f8ee80-SVC2] ` 加表中后缀，RFC Message-ID为 `<后缀-8f8ee80-SVC2-20261116@mailauto-qa.example.invalid>`。目录UIDVALIDITY分别为A=30、B=31、Target=32、Other=33；表中UID仅在对应目录/UIDVALIDITY下有效。

| 后缀 | 稳定ID SHA256前16位 | 当前远端QA成员及UID | flags / 本地与冻结证据 |
| --- | --- | --- | --- |
| G-EXISTING | aed7e635bb4a3f41 | Target:6、Other:6 | Seen；与索引及completed move冻结身份相符 |
| G-FLAGS | bc66af942e25c7aa | A:19 | Seen；与索引相符 |
| G-MULTI | b5a650bd5484f492 | B:9、Target:7、Other:5 | Seen；与索引及completed move冻结身份相符 |
| G-RULE | 206b8cf1806954ff | B:10、Target:8 | Seen；与索引及completed规则冻结身份相符 |
| G-STAR | 0b688f34f253e2b9 | A:18、B:12 | Seen+Flagged；与索引相符 |
| G-UNREAD | 1e409ed5a21e5634 | A:17、B:11 | flags为空，仍未读；与索引相符 |

三个unknown亦按冻结稳定ID只读核对：历史G-MULTI `4df4351006717085` 在A:1/B:1/Target:2/Other:1；历史G-EXISTING `34b023ae95dcdd22` 在A:2/Target:1/Other:2；SVC1 G-MULTI `522bc0ae66a7a9f3` 在A:8/B:5/Target:5/Other:3，均Seen且与索引一致。各自源A及目标Target同时存在，证明当前成员关系未达到原move完成后态，也不能判定完全未执行；**三条均证据不足以通过正式只读核验结案，继续needs_verification**。没有调用Reconcile：该接口除只读验证外还有本地状态发布及可能的FinishMove分支，本检查点未授权这些动作。未知写入不重放，busy不绕过；写入关闭与settings version变化的保护保留。

主代理独立网页新证据：搜索框和URL正确，结果6个会话行，行内显示多封Mail Auto QA；展开已读G-MULTI合成会话中23:43单封“显示详细信息”，官方详情主题确为 `[MA-QA-8f8ee80-SVC2] G-MULTI`，而会话标题仍显示旧811ab7a。**已实证Gmail会话聚合导致旧会话标题，并非SVC2不存在**；这是主代理取得的网页证据，本协作者未同时操作浏览器。与本次独立IMAP稳定身份结果共同澄清此前列表矛盾，不删除8.6/8.6.1的历史观察。会话级标签仍不能代替单封标签；六封逐项网页身份核对由主代理继续，不再要求用户解决已解释的旧标题疑点。

证据根目录 `.planning/service-qa-followup/`：`remote-readonly-result.json`含逐封完整合成主题/Message-ID、稳定ID摘要、UID/UIDVALIDITY/flags/时间及36项命中/未命中；`remote-readonly-before.json`记录备份及基线；`remote-readonly-after.json`记录与私有备份逐表比较、命令/退出码和证据摘要。实际 `go build -o .planning/service-qa-followup/remote-readonly .planning/service-qa-followup/remote_readonly.go` 退出0；运行 `.planning/service-qa-followup/remote-readonly` 退出0，build/run各自log和exit保留，不覆盖旧证据。未重复全量门禁，不将诊断编译当产品测试。

连接退出、数据库关闭后无WAL sidecar、18090无监听；11个业务表（operations/plans/items、messages、folder_settings及automation tasks/policies/batches/claims/daily、gmail_run_claims）与配套私有备份摘要一致。queued/running=0，三条unknown及冻结摘要不变，两个账号写入关闭、两个任务停用。没有新增合成邮件或改flags/标签，OAuth刷新仅经现有CAS路径处理授权元数据。停在A等待主代理复核；B/C、发布和部署仍未授权。

### 8.4 授权返回失败的独立核对与 Outlook 网页恢复

2026-11-16 晚间收到 Gmail 权限错误截图后，核对 Google 官方请求确实含 `openid email https://mail.google.com/`、offline、PKCE S256 和本机精确回调。21:59:07 浏览器历史留下 `oauth_scope_missing`，G-PERSONAL-01 仍 pending/generation1，无主体和 refresh。Google 账号的关联应用详情仅显示 Mail Auto Personal QA 获准 Google 登录/个人资料，未列 Gmail 数据访问。这是当前缺邮件授权的直接官方页面证据；没有保存原始 token 响应，不能声称已知其精确 scope 字符串，也未复现 scope 规范化误判。

补充正式回归覆盖 identity-only、规范化 identity scope、Gmail readonly 与缺 scope 的拒绝，以及正确 IMAP scope 与 identity 别名/空白组合的接受。两包 race 共 150 个叶子通过、0 失败/跳过，新增 8 项；相关 vet、格式及差异检查通过。只新增测试，不改变产品校验，不以 mock 替代真实 Gmail 验收。

MS-PERSONAL-01 仍 connected/generation4，成功授权审计最近为 19:34:39；晚间重新授权不能记为通过。22:06 的新 Manager 真实刷新后只读 IMAP 诊断通过，13 目录、MOVE/UIDPLUS。官方网页首次加载错误仍保留；按官方 Refresh 建议后恢复同一账号的 QA 目录：源 3 封、目标 3 封、存档 1 封。随后仅对 `[MA-QA-574b047] M-FLAGS-02` 补做未读/已读网页前后核对，新增两条 completed 操作；其他动作的逐项网页对照仍未全部补齐。任务/策略保持关闭，临时写入每次恢复关闭。

本轮起始打开 Outlook 默认收件箱时自动出现私人列表预览，随后立即切到 QA；未打开私人邮件，未将这些预览存入证据或报告。补充 JSON、实际 operation ID、退出码及仅 QA/授权页截图在 `.planning/oauth-consent-followup/`，对应实施交接第 15.7 节。原证据和失败记录均保留。

当前必须本人在 Google 官方页面明确授予 Gmail 邮件权限，之后才能继续 Gmail 实测；未代点账号选择或权限同意。Outlook 晚间同身份重新授权、其余网页动作对照、真实人工保留及所有 Gmail 验收仍未完成。**等待主代理独立验收；尚未发布部署。**

### 8.7 原账号重新授权后的只读复核（2026-11-17）

用户确认 Gmail 与 Outlook 均已对原账号重新授权。本轮 10:18:18–10:24:23(+08) 只读核对：本机 Mail Auto 管理卡片显示两账号 OAuth connected；受保护本地库 scope 与预期一致，Google 为 `openid`、`https://www.googleapis.com/auth/userinfo.email` 与 `https://mail.google.com/`，Microsoft 为 `https://outlook.office.com/IMAP.AccessAsUser.All`；两者应用origin均为 `https://localhost:18444`，回调分别为 `/api/mail-oauth/google/callback` 和 `/api/mail-oauth/microsoft/callback`。敏感主体仅以hash记录，登录标识与原绑定账号相符；generation较C1基线分别递增至3和5，config版本2和1。最近成功授权审计时间约10:04:51与10:05:39(+08)，未记录授权码、token或state；当前OAuth state表0项。两账号在10:18:23–10:18:26通过Mail Auto连接诊断，TCP、TLS、OAuth IMAP登录及LIST通过，Google 19目录、Outlook 13目录。诊断只用于身份连接，不执行同步。

对三条Gmail unknown，10:21:34–10:22:20通过独立只读IMAP helper在精确四个MailAuto-QA标签内，以RFC Message-ID与X-GM-MSGID双重搜索并只FETCH envelope/flags/X-GM-MSGID/X-GM-LABELS元数据；均与原账号计划稳定ID匹配。当前三封均仍位于原源标签、目标MailAuto-QA-Target及相应QA额外标签，flags为Seen。原操作状态继续 `needs_verification`、错误为 `remote_state_unknown`、恢复码仍为 `recovery_account_changed`，计划SHA256与C1冻结值不变。没有调用Reconcile/FinishMove、没有重放/取消/删除/手改，队列为空、邮箱写入和任务关闭。该只读观察说明当前成员状态，不结案也不推断历史动作实际结果。

Outlook授权和IMAP连接通过。针对官方网页仅直接进入 `https://outlook.live.com/mail/`；页面报 `TrustedTypePolicy` 的default policy已存在及TrustedHTML赋值错误，点击网页官方“Refresh the application”后仍失败。没有打开收件箱或任何邮件，故QA文件夹及测试邮件的网页端前后状态本轮无法确认；不推断登录故障或账号当前不可用。已有索引显示此前授权范围为全部可读取目录、Outlook索引130条，其中QA目录成员6条、QA主题前缀记录7条；本轮未触发同步，不能把旧索引说成当前网页状态。

人工保留方面只读计数 `manual_keep_blocks=0`、`manual_resolutions=0`，没有真实人工保留夹具。本轮不伪造记录、不人工结案；此项未验证。源码边界为：Gmail标签是同一逻辑邮件，“保留两份”在服务端明确拒绝；Outlook普通IMAP账号只有在源/目标为不同位置、Message-ID与主题/发件人/时间/大小等冻结元数据吻合、远端DuplicateVerifier确认字节摘要/大小/flags一致且不存在冲突操作时，才可人工保留结案。本轮没有Outlook unknown夹具，因此该路径没有实测。Gmail专用只读helper明确记录fresh OAuthManager真实refresh获取凭据成功；Microsoft只读连接使用新建OAuthManager后IMAP成功，但没有单独保留refresh响应/轮换字段，故不宣称refresh token发生轮换。凭据及响应正文均未记录。组织Google Workspace/Microsoft 365仍未验证。

脱敏详细证据 `.planning/real-account-followup/summary.json`、`audit-before-web.json`、`current-gmail-unknown-remote.json`；只读IMAP Go helper源码/二进制留在同一私有planning目录，不纳入源码镜像。未读取私人邮件正文；未执行Sync、写入、任务或自动同步。**本轮部分只读账号验收通过；Outlook网页对照、真实人工保留、unknown安全结案路径及其余真实操作矩阵仍未完成，等待主代理独立审计，尚未发布部署。**

### 8.8 最终补证：fresh-manager、逐封保护及安全拒绝（2026-11-17）

本节不覆盖8.7的失败或历史观察。任务入口为 `docs/gmail-outlook-final-remediation-task.md`；起始HEAD `92d3b7326e9c467522817a409ab9d1f2afbc3534`，产品候选仍为 `ac29317395f21cf12a9b006e2adc2f4eec3eea60`、0.10.4/schema15，没有产品代码或迁移变更。实际命令、退出码、叶子计数、证据SHA256及最终快照汇总于 `docs/gmail-outlook-final-remediation-evidence.json`；原始私有证据在 `.planning/final-remediation/`，不提交或放入镜像。

#### 8.8.1 已真实通过与逐封证据

- **真实refresh而非改过期时间**：11:42:04–11:42:09(+08)，独立helper进程为每账号显式新建 `mailoauth.Manager`，无access-token缓存，调用正式 `CredentialForRoute`。成功路径必须实际完成提供商refresh grant及既有加密CAS写回，随后同一Manager作为生产 `Adapter.DiagnoseRoute` 的AuthSource，TLS、SASL **XOAUTH2**和只读LIST通过。MS-PERSONAL-01返回13目录，Google返回19目录；两者主体/tenant、generation(5/3)、应用版本(1/2)、scope及connected状态保持不变。未修改过期时间、未打印token/响应、未声明refresh token轮换。此为新helper真实刷新与登录证据，不冒称运行服务的缓存刷新或又一次服务重启。
- **正式Service保护预览**：11:41:39，活动数据库以mode=ro/query_only打开，调用现有Gmail QA任务的 `Service.PreviewTask`，不保存任务/策略、不调用RunTask。SVC2 G-STAR与G-UNREAD在A/B标签各有一条预览，共4成员、2逻辑邮件；不是4封独立邮件，也不是新任务run的保护总数。

| 精确SVC2夹具 | 稳定ID SHA256 | 预览/远端成员 | 正式预览原始原因 | 本轮远端flags |
| --- | --- | --- | --- | --- |
| G-STAR | `0b688f34f253e2b9709956dd58faf493fdaad329c5361fff83eed516cb1cf651` | MailAuto-QA-A、B | `Gmail 其他标签视图受保护：保留星标邮件`，status=protected、action=keep | 已读、星标、flags_known=true |
| G-UNREAD | `1e409ed5a21e56342695caee27c528eaa82033797e9682c05e9af0bc4cbe8fe1` | MailAuto-QA-A、B | `Gmail 其他标签视图受保护：保留未读邮件`，status=protected、action=keep | 未读、非星标、flags_known=true |

11:47:12–11:47:57以生产 `Adapter.RuleState` 对六封现有SVC2合成邮件的12个已知QA成员只读EXAMINE/UID FETCH元数据，身份及flags均与索引一致；只涉及四个既有QA标签，不调用Sync、不发布索引、不读正文。STAR/UNREAD各两个成员均与上述预览对应；UNREAD没有在网页打开，没有隐含Seen写入。MULTI为B/Target/Other，EXISTING为Target/Other，RULE为B/Target，FLAGS为A；这些是当前成员，不补造历史动作前后状态。

#### 8.8.2 Gmail网页身份与能力边界

较早同任务在用户既有Chrome中，精确RFC Message-ID搜索并查看G-MULTI、G-RULE单封原始邮件页；续接记录保存了原始主题/Message-ID及permmsgid十进制字符串，但没有留存那次完整原始头或截图。本轮仅整理已取得观察并与原IMAP证据及上述当前RuleState回读交叉计算，**不是重新打开网页完成全部网页验收**：

| 夹具 / 精确Message-ID | 观察到的网页permmsgid尾号（字符串） | SHA256与X-GM-MSGID |
| --- | --- | --- |
| G-MULTI / `<G-MULTI-8f8ee80-SVC2-20261116@mailauto-qa.example.invalid>` | `1882030162988661164` | `b5a650bd5484f4927c22d50e30643413d634e3ee02c5d8b38511333e8c4010d5`，一致 |
| G-RULE / `<G-RULE-8f8ee80-SVC2-20261116@mailauto-qa.example.invalid>` | `1882030167238756650` | `206b8cf1806954ff5402e3b2ff5bdcfac52606b6ab39b24202a5674d5a700b65`，一致 |

不能把会话聚合的旧标题或会话标签并集当成单封状态；成员关系采用逐封稳定ID回读。另三封已读样本只有初步单封详情观察，精确原始头证据未补齐；UNREAD网页未打开，保留只读IMAP证据及网页限制。

活动只读库调用正式 `NewPlan`：QA源归档、Trash、恢复及7个SPECIAL-USE目标移动均返回 `gmail_action_unsupported`，没有生成计划；对一条现有已完成QA普通move调用 `UndoPlan`，返回 `gmail_undo_unsupported`，没有反向计划。未调用三条真实unknown的结案/核验方法。Gmail“保留两份”另外在临时SQLite、现有正式夹具与核心Worker路径的补充overlay测试中证实 `PreviewManualKeep` 返回 `gmail_keep_both_unsupported`，原模拟unknown及计划不变、无第二次move/FinishMove；这是**模拟拒绝通过**，不是现有真实unknown的人工结案，也不是已纳入默认门禁的新产品测试。INBOX不在四QA标签范围，成功归档只引用正式协议模拟回归，不增加真实范围。

#### 8.8.3 人工保留与网页阻塞

现有正式Go race定向回归59叶子通过、0失败/跳过（13个顶层测试），涵盖人工保留原子/幂等/永久保护、变化证据拒绝、并发/预排队冲突、重启/重索引、生产network.Manager直连/代理身份变化，以及IMAP完整BODY.PEEK[]、flags/UIDVALIDITY/断线等fail-closed协议检查；另补充Gmail保留两份overlay为1/0/0，不与59或历史全量642累计。

Outlook真实unknown人工保留仍**未实测**。安全隔离设计：新临时SQLite/Vault、纯合成账号及唯一测试Worker；正式Service/Worker发起一次受控COPY，由本地fake IMAP确定性省略可信COPYUID或丢失最终确认，保留源；之后正式Preview/Resolve只做完整原文PEEK及身份/flags复核，检查双份保护、审计和任务计数原子提交、预备计划拒绝及重启后持久性。不得复制真实token、在现有QA库SQL造unknown或破坏真实连接。现有Service核心Worker夹具与fake协议测试已实际执行；**新的fake-TCP+后台Worker组合端到端夹具只完成设计，没有实现或冒称运行，更没有真实Outlook供应商结案结果**。是否另做隔离真实供应商测试由主代理决定。

用户随后明确只使用其现有Chrome：已正常关闭先前独立Puppeteer/Chrome，不再使用或创建独立profile，保留旧失败证据。既有Chrome157.0.8103.79有Gmail/MailAuto标签，没有Outlook邮件标签；AppleScript JavaScript执行关闭，返回error12，未开启设置或绕过策略。不会为了网页阻塞中止其他验证。

**USER-ACTION-REQUIRED**：本人在正在使用的Chrome打开Outlook官方页面，进入原绑定个人账号现有 `MailAuto-QA` 目录；如需登录由本人完成。无需聊天提供密码/token/secret；不要求重复OAuth授权。其余网页逐动作对照仍待验。

#### 8.8.4 最终安全终态及不能忽略的时间戳差异

11:23:46先通过SQLite backup API制作一致数据库及配套主密钥私有备份，目录0700、db/key0600；运行库读取使用mode=ro而非immutable。后续及最终实时快照确认唯一PID85404/原候选二进制，ready/0.10.4/schema15，24 completed、3 needs_verification、queued/running=0，写入、两个任务、自动策略、自动同步及MCP均关闭；两主体/scope/代际、四QA标签、三份冻结计划SHA及稳定身份未变。最终采样时间见机器证据，不将本节较早时间冒称最后时刻。

加强配套备份全表比对时首次断言退出1：三条unknown的 `operations.updated_at` 和对应 `operation_items.checked_at` 随现有唯一Worker周期保守拒绝而前进。逐行检查仅这六个单元格不同；`reconcileNextOperation`领取时更新updated_at，当前指纹不符在密码/远端调用前拒绝，`recordRecovery`写回原recovery code/detail及checked_at。未由helper调用Reconcile、未新增Worker、未进行provider邮件写入，原state/error/recovery、取消标记、源目标映射、其他时间列和冻结计划均严格相同。

保留首次失败日志。最终验证**不声称原表逐字一致**：机器索引明确列出这两列逐记录旧/新值、raw-table-matches=false，并严格比较其余所有列；accounts/messages/mailboxes/folder_settings/mail_rules/automation_tasks/automation_policies/operation_plans/automation_claims/manual_keep_blocks/manual_resolutions原表摘要均一致。生产OAuthManager正常加密refresh CAS写回是另行授权的凭据维护，不当作业务变更，也不据密文变化推断供应商轮换。

额外全库逐列审计退出0：63个用户表schema/主键集合一致，无插入/删除；60表原表完全一致。仅上述两列各3个诊断单元格及两账号 `mail_oauth_accounts.refresh` 加密CAS密文列变化；所有其他列（含其他时间字段）相同。敏感值不输出，只记录变化列名、行键hash及值hash，详见 `database-column-diff.json` 与机器索引。此项不是凭据轮换证明。

FR-01继续保守拒绝：三份v2计划只在重建generation2/config2时匹配，当前为3/2；冻结设置版本1/3/9对当前18，旧计划没有独立稳定主体/tenant快照，不能凭同邮箱地址或当前标签并存放宽。三条未知操作不取消、不删除、不重放、不解除busy。新增真实写入/操作/run/邮件均为0；没有推送、发布标签、镜像发布、部署或生产清理。**部分真实只读补证与模拟/能力拒绝通过；真实账号整体验收未完成。等待主代理独立验收；尚未发布部署。**

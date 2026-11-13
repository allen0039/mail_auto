# 0.6.0 验证范围

日期：2026-11-13（Asia/Shanghai）。本版新增显式代理和诊断，数据库 schema 12。

## 自动化验证

| 范围 | 对应测试 |
| --- | --- |
| 全局默认直连、继承/覆盖、凭据加密与脱敏、乐观锁、引用保护、凭据变化代际 | `network.TestPoliciesEncryptionReferencesAndNoEnvironment` |
| 主机/配置验证、Agent 出口变化的事务围栏、TG 变化不影响 Agent | `network.TestProxyValidationAndTransactionalAgentFence` |
| HTTP CONNECT、远端域名传递、认证与错误脱敏 | `network.TestHTTPConnectTunnelRemoteDNSAndAuthentication` |
| SOCKS5 用户名密码、目标域名远端解析与隧道 | `network.TestSOCKS5RemoteDNSAndAuthentication` |
| HTTP/HTTPS/SOCKS5 失败不直连、直连忽略环境变量 | `network.TestFailedProxyNeverFallsBackAndDirectIgnoresEnvironment` |
| HTTPS 代理证书拒绝、CONNECT 取消、受信任证书下真实 TLS 代理隧道 | `network.TestHTTPSProxyCertificateAndCancellation`、`TestHTTPSProxyTrustedTunnel` |
| 邮箱代理更新不清索引、全局更新推进继承账号的后台连接代际 | `mail.TestAccountProxyPreservesIndexAndGlobalRevision` |
| 真实 TLS IMAP 登录、目录、扫描、正文与自动同步全部通过实际 CONNECT 隧道 | `imap.TestIMAPTLSAndAutomaticSyncThroughProxy` |
| Agent 目标限制、显式信任远端 DNS、本地 DNS 默认验证 | `modelprovider.TestProxyDestinationGuardAndTrustedRemoteDNS` |
| TG 共用出口，验证/轮询/发送/编辑/回调经实际 CONNECT 隧道，绑定与通知版本保持 | `telegram.TestSharedProxyCoversAllBotMethodsAndPreservesBinding` |
| 管理 API 鉴权、CSRF、凭据脱敏、旧版本冲突、失败诊断不回退 | `admin.TestNetworkAPIAccessRedactionAndDiagnosticFailure` |
| schema 11→12 保留邮箱与索引，网络设置初始为空 | `store.TestSchema11UpgradePreservesBusinessData` |

完整既有 Go 测试继续运行，包含原邮箱读写、写入不明后的核验、任务保护、Agent 确认与授权围栏；另运行 race、vet、格式、staticcheck、govulncheck、Compose 验证和 linux/amd64、linux/arm64 构建。浏览器检查覆盖新代理配置保存/回显、单账号/TG/Agent 选择、错误诊断、直连与代理对比及移动端。

## 验证边界

协议测试使用本地假代理、假 TG 和临时 SQLite；不能替代用户 NAS 网络和实际代理服务商的端口策略验证。真实模型或 TG 尚未配置时，不为了验收而启用功能或填写虚构凭据。生产发布需另核对版本、摘要、健康、重启数、邮箱文件夹同步与实际任务记录。代理不能保证解决授权失败、服务端限流或 IMAP 能力问题。

Agent 显式信任远端 DNS 时，最终连接目标的 IP 限制由可信代理负责；本地仍验证 HTTPS 证书及输入的目标地址限制，此模式不是本地 IP 固定校验的等价替代。

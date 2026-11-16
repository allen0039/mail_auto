# Google / Microsoft 应用注册一次，供自用部署复用

应用复用于 0.10.1 引入，当前开发候选为 0.10.3，schema 15，无新增迁移。真实 Gmail / Outlook 整体验收待完成；本候选等待主代理独立验收，尚未发布部署。

同一个服务商客户端可以供多个本人管理的 Mail Auto 实例使用，每个邮箱分别官方登录和同意。保留数据库和配套主密钥重建容器，本来就会保留应用配置及有效邮箱授权。新建实例可以使用本页的部署文件自动载入现有客户端，省去重新创建应用和逐个填写密钥。

这是自用应用复用功能。源码、前端、公共 Compose 和镜像不包含真实 client secret；没有新增代管他人令牌的授权中转服务器。全新第三方安装仍须有自己的应用或由可信管理员私下提供配置。不同域名的精确回调仍必须提前加入服务商允许列表；普通 Web OAuth 客户端不能接受任意回调或通配域名。Google Testing 的测试用户及七天 refresh token 限制继续适用。两家应用可分别配置和复用，未配置的服务商不能用示例值冒充成功。

## 第一次配置与导出

1. 管理员在服务商注册应用并在 Mail Auto 的受保护设置中保存，每个服务商一次。Google 与 Microsoft 的客户端互不通用；注册范围及个人账号类型见 [接入说明](gmail-outlook-setup.md)。
2. 在宿主机使用与该实例一致的数据库及主密钥目录导出到仓库外的私有目录。父目录建议 0700，不要把导出文件放入 Git、备份公开目录或镜像上下文。命令示例中的目录须替换为实际私有目录：

```sh
mail-auto --data-dir /private/mail-auto/data --key-dir /private/mail-auto/keys mail-oauth export /private/mail-auto/oauth-apps.json
```

也可在容器内导出到仅该容器用户可写的私有目录，再通过受保护的运维方式复制。CLI 不提供终端输出模式，也不覆盖已有文件。文件默认 0600，包含两家已配置应用的 client ID、明文 client secret、账号类型和密钥到期时间；不包含邮箱 refresh/access token、账号身份、origin 或主密钥。它是敏感部署 secret，不是可以公开分享的配置示例。

## 后续部署自动载入

将同一私有文件安全传输并只读挂载到新实例，例如 `/run/secrets/mail-oauth-apps.json`。文件只允许所有者读取（0400 或 0600），且必须为普通文件；运行用户必须有读取权限。官方容器用户 UID/GID 为 65532，可设置文件所有者为 65532、模式 0400；Compose 本地 file secret 通常保留宿主文件权限。不能使用符号链接、目录或世界可读文件。

私有实例 YAML 配置：

```yaml
public_url: https://mail.example.com
mail_oauth_apps_file: /run/secrets/mail-oauth-apps.json
```

或使用参数，参数只包含路径，不传明文 secret：

```sh
mail-auto --public-url https://mail.example.com --mail-oauth-apps-file /run/secrets/mail-oauth-apps.json serve
```

先在 Google / Microsoft 原应用中登记新实例精确回调：

- `https://mail.example.com/api/mail-oauth/google/callback`
- `https://mail.example.com/api/mail-oauth/microsoft/callback`

启动时在任何同步、整理 worker 或 HTTP 服务启动前载入文件。origin 只从经过验证的 HTTPS public_url 获取，不从浏览器 Host 推导。两个服务商在同一个短 SQLite 事务中写入，在目标实例 Vault 下重新加密；任一失败全部回滚并拒绝启动，不进行供应商网络请求。缺失的服务商不删除或覆盖现有配置。文件格式版本固定为 1，最多两家服务商，最多 32 KiB，未知字段/服务商、额外 JSON 文档、空或已过期密钥配置均拒绝，错误不包含原文件或数据库敏感响应。

配置成功后，在管理界面核对“应用已配置”和精确回调，再添加邮箱并在官方页面登录同意。新实例不会从部署文件获得旧实例的邮箱授权，每个邮箱仍需分别同意。组织账号未验证，不能推广为所有 Workspace / M365 租户可用。

## 重启、轮换和回退

相同应用、origin、账号类型、密钥及到期时间重复启动不增加配置版本、不清除正在进行的 state、不作废缓存或现有 refresh token。同一应用轮换密钥/到期信息时增加版本、作废旧 state/cache/连接，保留原邮箱 refresh token；修改 client ID、origin 或账号类型则按现有机制清除旧 refresh token，要求重新授权。离线导入发生在启动前，不会混用新旧 worker。

启用了部署文件时，它是每次启动的配置来源；在管理界面改过应用后，须重新导出更新部署文件，或移除 mail_oauth_apps_file 后改由数据库配置管理，避免重启恢复旧设置。删除部署文件配置不会删除数据库里已经加密保存的应用。每次部署仍须保留数据库及配套 master.key；部署文件只能恢复应用配置，不能替代完整备份。

回退本候选时先停止服务，移除旧二进制不认识的 mail_oauth_apps_file 参数/YAML 字段；0.10.0 返工候选可使用相同 schema 15，但必须具有 GO-IA-01～04 修复，不能回退到缺少保护的初次候选。跨 schema 15→14 仍须恢复配套数据库和主密钥快照，不能仅更换二进制或手改 user_version。配置回退可能需要邮箱重新授权，不会撤回已执行的远端动作。

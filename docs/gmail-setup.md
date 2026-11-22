# Gmail 接入与迁移

1. 在 Google 账号开启两步验证，确认账号允许使用[应用专用密码](https://myaccount.google.com/apppasswords)。不要使用 Google 登录密码。
2. 在 Mail Auto 的「添加邮箱」填写 Gmail 邮箱地址和应用专用密码，可直接粘贴带空格的分组密码。
3. Mail Auto 通过 `imap.gmail.com:993` 严格校验 TLS、登录和 `X-GM-EXT-1` 能力，验证成功后加密保存。更换密码时点击「编辑配置」；验证失败不覆盖旧密码，留空可保留已有密码。

## Gmail 标签与整理限制

- 支持标记、普通标签分类及从 INBOX 归档到 Gmail 识别的「所有邮件」，保留无关标签。唯一邮件按 `X-GM-MSGID` 去重，不等同于各文件夹条目数。
- 分类或归档前，应在整理设置中探测文件夹用途。回收站、恢复、特殊目录移动、移动或归档撤销及「保留两份」不支持。未知写入不会自动重放。

## 数据库迁移与回退

- `0.10.6` 使用 schema 17。升级删除 Gmail 和 Outlook 的本地 OAuth 应用配置、绑定、刷新令牌及待处理授权状态。MCP OAuth、其他邮箱授权码、Gmail 标签映射以及原有索引、规则和任务不受影响。
- 旧 Gmail OAuth 邮箱需要在原账号的「编辑配置」补填应用专用密码。有应用专用密码的邮箱无需重新配置。任何已有 Outlook 记录都只保留本地索引；不会再连接、同步或运行整理任务。
- 从旧候选升级前，先从私有 YAML、Compose 和启动参数移除 `mail_oauth_apps_file`、`--mail-oauth-apps-file` 及相应的 secret 文件挂载。确认不被其他服务使用后，可清理宿主机上的旧 OAuth 部署文件。迁移不操作外部文件或已有备份。
- 升级前用旧版 `mail-auto backup PATH` 保存数据库，并另行保护配套 `master.key`。旧二进制无法打开 schema 17；回退须停止容器，恢复升级前的数据库快照、主密钥和旧镜像，不要手改 schema 版本号。

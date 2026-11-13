# 运维、备份与恢复

在保存 `compose.yaml` 的 Docker 主机上定义辅助函数：

```bash
ma() { docker compose "$@"; }
```

本页使用默认项目名 `mail-auto`；若通过 `-p` 自定义项目名，请保持运维命令使用同一项目。

## 运行与访问

执行 `ma pull`、`ma up -d --no-build`，访问 <http://127.0.0.1:8080>。远程访问可通过 SSH 转发；公网部署使用 HTTPS 反向代理。镜像运行 UID/GID 65532，无 shell、非 root、只读根文件系统，使用独立数据与密钥卷；内置健康检查通过二进制本身执行。审计存 UTC Unix 时间。

查看 `ma ps`、`ma logs --tail 100 app`。日志不记录请求正文、授权码或登录令牌。`/healthz` 仅返回进程状态和版本；`/readyz` 额外检查 SQLite 和主密钥是否可用，两者都不包含邮箱数据。Compose 健康检查使用 `/readyz`。MCP 默认关闭，启用方式见 [MCP 接入指南](mcp-setup.md)。

HTTPS 代理需配置 `public_url: https://mail.example.com` 与 `secure_cookie: true`，代理保留 Host。基础 Compose 仅绑定服务器回环地址。服务不信任任意 X-Forwarded-* 标头；登录限流按直接对端 IP 计，代理后的管理员共享限流。Origin 必须精确匹配 public_url；不提供通配 CORS。HTTPS 设置后，HTTP 下的浏览器不会发送会话 Cookie。

## 管理员恢复

```bash
ma exec app mail-auto bootstrap recover
ma exec app mail-auto bootstrap show
```

第一条命令立即撤销全部会话、使旧密码失效并生成新临时凭据；第二条主动显示它。成功使用一次后不可再次查看；若在设置前丢失会话，再执行恢复。恢复只通过宿主机 CLI，不提供匿名 HTTP 重置入口。恢复不改动加密主密钥、邮箱连接或元数据。

临时密码过期和会话过期以持久化绝对时间计算，重启不会延长。正常会话绝对有效 12 小时，设置会话 30 分钟。正式登录后，进入「系统设置 → 登录账号与密码」，验证当前密码后修改用户名或密码。新密码留空则只修改用户名；填写新密码须至少 12 个字符、最多 256 字节，且不能与当前密码相同。保存后立即撤销所有设备会话并重新登录。忘记当前密码时仍使用上述宿主机恢复流程。账号修改接口每 5 分钟最多尝试 5 次，并受同源、CSRF 和密码哈希并发限制保护。

## 0.6.0（schema 12）升级与回退

- 升级前做一致性数据库备份，并单独备份配套主密钥与部署配置。schema 11→12 只新增三张网络配置表，既有邮箱索引、任务、规则、策略、TG Token/绑定/接收位置和待核验记录不变。
- 全局默认直连，各邮箱、TG、Agent 默认跟随全局。业务连接不再隐式读取环境代理；此前通过容器 `HTTPS_PROXY` 访问 TG 的实例，升级后须在「网络与代理」保存并选择代理。
- 保存代理后无需重启容器。继承全局的邮箱跟随全局有效出口重连；指定代理失败不自动直连。模型有效出口变化会暂停 Agent 并使旧检测、未完成轮次和方案失效，须重新测试聊天/工具闭环后启用；独立固定任务不受影响。
- 0.5.1 及更早二进制拒绝打开 schema 12。回退须停止服务，恢复升级前快照、配套主密钥和旧镜像；不要手工改 `user_version`。回退不撤销远端已完成写入。
- 升级后先核对版本、健康、重启数、邮箱诊断、全部文件夹同步和自然任务结果；验证通过后再清理本项目旧文件和镜像。见 [代理说明](network-proxy.md)。

## 0.5.1（schema 11）升级与回退

- 升级前照常做一致性备份并单独备份 `master.key`。schema 10→11 只为 Agent 方案、Telegram 回复和 inbox 各增加一个整数列（执行围栏用），并把旧版本留下的未完成方案标为失效、未发送回复标为抑制；邮件、规则、任务、策略、通知和待核验记录不变。新功能仍默认关闭。
- 升级后 0.5.0 及更早二进制会拒绝打开数据库。回退须停止服务，恢复升级前快照与配套主密钥；不要手工改 `PRAGMA user_version`。回退不撤销已发生的原邮箱移动。
- 从快照恢复后仍须在启动 `serve` 前离线执行 `mail-auto bootstrap recover`。

## 0.5.0（schema 10）升级与回退

- 升级前用下方命令做一致性备份，并单独备份 `master.key`。schema 9→10 只新增内置 Agent 与 Telegram 交互的表；既有任务、规则、策略、通知设置、文件夹映射、邮件元数据与待核验记录不变。新功能默认关闭：升级后不会轮询 Telegram、调用模型、绑定或写邮箱。
- 升级后数据库为 schema 10，0.4.x 二进制会拒绝打开。回退时停止服务，恢复升级前的数据库快照及配套主密钥，再启动旧镜像；不要用旧镜像直接打开升级后的数据库，不要手工修改 `PRAGMA user_version`，也不要把升级后的 WAL 混入旧备份。
- 回退会丢失升级后的本地记录（Agent 会话、方案、Telegram 绑定和升级后产生的运行记录），但不会撤销已经发生的原邮箱移动；回退后重新同步并人工核对远端状态，不要重放旧任务。
- 若以后从 0.5.x 的快照恢复，**在启动 `serve` 之前**离线执行 `mail-auto bootstrap recover`：它会撤销快照中的 Telegram 绑定、未执行的 Agent 方案与排队轮次，使未用绑定码失效，并建立新的接收起点；快照之后积累的 Telegram 命令不会自动执行。恢复后需在 Web 重新生成绑定码绑定。
- 模型服务或 Telegram 不可用不影响 `/readyz`、邮件同步、Web、MCP 和既有固定任务。

## 0.4.20（schema 9 不变）升级与回退

- 0.4.20 不修改数据库结构，仍为 schema 9，无迁移。升级前仍按下方命令做一致性备份，并单独备份 `master.key`。
- 本版只改变待核验移动的自动核验与自动同步之间的协调：只读核验不再使进行中的同步失效；发布恢复结果或尝试源清理前仍使同步快照失效。
- 回退到 0.4.19 可直接用旧镜像打开同一 schema 9 数据库，建议先停止服务并保留升级前备份。回退后，存在长期待核验移动的账号可能再次出现自动同步反复放弃的问题。

## 0.4.19（schema 9 不变）升级与回退

- 0.4.19 不修改数据库结构，仍为 schema 9，无迁移。升级前仍按下方命令做一致性备份，并单独备份 `master.key`。
- 本版只改变自动整理的领取资格：系统因任务期限或工作租约到期、且未发出写入就取消的邮件，可在下一轮重新评估；已有操作记录、取消来源和目标 UID 映射不会被改写。
- 回退到 0.4.18 可直接用旧镜像打开同一 schema 9 数据库，但仍建议先停止服务并保留升级前备份。回退后，系统停止且未尝试写入的邮件会恢复为 0.4.18 的行为，即不再被自动领取。

## 0.4.18（schema 9）升级与回退

- 升级前用下方命令做一致性备份，并单独备份 `master.key`。schema 8→9 只追加列：已有操作、目标 UID 映射和取消标记保持不变；升级前已取消的操作没有停止来源，一律按手动停止处理，不会被自动补完。
- 升级后数据库为 schema 9，0.4.17 及更早的二进制会拒绝打开。回退时必须停止服务，恢复升级前的数据库备份及配套主密钥，再启动旧镜像；不要用旧镜像直接打开升级后的数据库，也不要把升级后产生的 WAL 混入旧备份。
- 回退会丢失升级后的运行记录和核验结果；旧版本对已结束任务的待核验项仍无法自动补完。

## 一致性备份

应用使用 SQLite WAL，运行中不要只复制主 `.db` 文件。使用 VACUUM INTO 创建一致性快照：

```bash
mkdir -p backups
ma exec app mail-auto backup /var/lib/mail-auto/data/backup-01.db
ma cp app:/var/lib/mail-auto/data/backup-01.db ./backups/backup-01.db
# 将主密钥单独放在另一个受保护的备份位置
mkdir -m 700 -p /your/separate-key-backup
ma cp app:/var/lib/mail-auto/keys/master.key /your/separate-key-backup/master.key
chmod 600 ./backups/backup-01.db /your/separate-key-backup/master.key
```

目标快照文件必须不存在。数据卷内的快照导出后可通过挂载卷的维护工具删除；镜像不带 shell。备份本身包含敏感索引、会话与配置，离机保存时使用用户自己的加密备份设施，数据库与密钥分开管理。外部 secret 原文件另行备份。本版不提供备份加密器或定时备份。

## 联合恢复

先保留现有卷的备份。停止 app，向一个新的/干净的数据卷放入快照并命名 `mail-auto.db`，向配套密钥卷恢复原始 `master.key`。不要把旧 WAL/SHM 文件与恢复的数据库混用。

以下命令示例创建恢复用 Compose 项目（需当前目录已有配置及已拉取的镜像），不会修改默认项目卷：

```bash
docker volume create mail-auto-restored_mail-data
docker volume create mail-auto-restored_mail-keys
# 将绝对备份目录中的文件复制进新卷；alpine 是一次性维护容器。
docker run --rm --user 0 \
  -v mail-auto-restored_mail-data:/restore \
  -v "$PWD/backups:/backup:ro" alpine:3.23 \
  sh -c 'cp /backup/backup-01.db /restore/mail-auto.db && chown -R 65532:65532 /restore && chmod 700 /restore && chmod 600 /restore/mail-auto.db'
docker run --rm --user 0 \
  -v mail-auto-restored_mail-keys:/restore \
  -v /your/separate-key-backup:/backup:ro alpine:3.23 \
  sh -c 'cp /backup/master.key /restore/master.key && chown -R 65532:65532 /restore && chmod 700 /restore && chmod 600 /restore/master.key'
# 先停止默认实例，释放同一宿主端口
ma stop app
docker compose -p mail-auto-restored -f compose.yaml up -d --no-build
# 撤销备份内可能仍有效的旧会话，并重新设置管理员
docker compose -p mail-auto-restored -f compose.yaml exec app mail-auto bootstrap recover
docker compose -p mail-auto-restored -f compose.yaml exec app mail-auto bootstrap show
```

保留两套卷直到确认恢复成功。数据库有初始化记录但密钥缺失、错误或权限过宽时，服务停止启动；它不会静默更换密钥。密钥不可恢复时，加密的邮箱授权码与临时引导记录不可解密。仅丢失管理员记录也不会自动创建新账号，应恢复有效数据库备份。

在服务器直接运行 Go 二进制时，恢复也遵循相同规则：停止进程，恢复数据库及密钥到所选目录，保证权限，再运行服务和恢复命令。SQLite 备份不备份服务商侧原始邮件。

## 升级与回退

升级前记录镜像 ID/标签，执行一致性数据库备份并单独备份主密钥及外部 secrets；保留旧镜像。保存升级前 `compose.yaml`、自定义覆盖文件和 YAML 配置。更新 Compose 中的镜像版本后执行 `ma pull`、`ma up -d --no-build`，检查健康状态和版本。

不能假定更换旧镜像即可回退数据库迁移。若新版本迁移不向后兼容，停止服务，使用升级前数据库快照、匹配密钥和旧镜像在单独项目卷中恢复后验证。全历史同步更新后的 0.1.0 schema 为 2（新增暂存索引和同步任务表）；发现比自己更新的 schema 会拒绝打开。当前迁移不提供 down。

`ma down` 保留命名卷；`ma down -v` 明确删除数据和密钥，仅在决定清空实例时使用。


## 全历史同步升级

升级前必须备份数据库和独立密钥。schema 1 → 2 会把已有连接的文件夹范围切换为全部可读取文件夹，保留账号、授权码、管理员、会话及现有索引。更新后点击「同步邮箱」补齐历史元数据；系统不会在升级时自动连接邮箱。

每个文件夹同步失败时保留其旧索引；任务状态会显示错误并允许重试。服务重启不会自动重新执行任务，页面显示「同步中断，可重试」。回退旧镜像需恢复升级前数据库，不能用 schema 1 二进制直接打开 schema 2。


## 删除索引后的空间回收

服务自动分批回收 SQLite 空闲整页并尝试截断 WAL，每分钟补查；无需每次删除后手动 VACUUM。旧库首次升级会执行一次格式转换与压缩，保留账号、凭据和邮件引用。数据库基本结构和页内少量余量仍占空间。升级快照和历史日志独立保留，删除在线索引不会改写旧备份。


## 0.2.1 原邮箱分类升级

schema 4→5 新增持久化 MCP 设置，不重置邮箱映射、逐账号写入开关或会话。页面 MCP 设置优先于旧启动参数的 MCP 初值，公开地址可留空关闭。更换域名会撤销客户端授权；已有旧版 OAuth 授权需重新登录。备份与回退须保留数据库和配套主密钥，旧版拒绝打开 schema 5。

## 0.3.0 规则与外部批次升级

schema 5→6 增加规则及授权、批次、领取和额度，新增收信日期与星标等元数据，由后台同步补齐。升级不创建规则/授权、不开放原邮箱写入，自动化默认关闭。旧二进制拒绝打开 schema 6，回退必须停服务后恢复升级前数据库、配套密钥、Compose 和旧镜像。本轮隔离验证与回退注意事项见[验收记录](archive/acceptance-0.3.0.md)。真实外部任务单独在支持 MCP 的客户端创建；页面说明不能替代任务创建。

回退前先停止服务，恢复升级前的配套快照与主密钥，避免混用升级后 WAL。恢复快照会放弃升级后的本地变更；数据库回退不会撤销原邮箱已完成的移动。

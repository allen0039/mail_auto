# 依赖与许可证

工具链固定 Go 1.27.3；go.mod / go.sum 固定 Go 依赖与校验值。Docker 使用多阶段纯 Go 交叉构建，运行镜像为 distroless static-debian13 nonroot，包含系统 CA，时区数据嵌入二进制。基础镜像标签可随维护更新，发布前应按自己的制品管理流程固定 digest 并重新扫描。

| 直接依赖 | 固定版本 | 用途 | 许可证 |
| --- | --- | --- | --- |
| github.com/emersion/go-imap | v1.2.1 | 稳定 v1 IMAP 客户端，与业务隔离 | MIT |
| github.com/emersion/go-imap-id | v0.0.0-20190926060100-f94a56b9ecde | NetEase 等服务的可选 ID 扩展 | MIT |
| github.com/emersion/go-message | v0.18.2 | MIME / 字符集 | MIT |
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | Streamable HTTP MCP 与工具 schema | MIT |
| golang.org/x/crypto | v0.58.0 | 仅使用 Argon2id | BSD-3-Clause |
| golang.org/x/net | v0.61.0 | HTML tokenizer | BSD-3-Clause |
| gopkg.in/yaml.v3 | v3.0.1 | 严格 YAML 配置 | MIT / Apache-2.0（按文件） |
| modernc.org/sqlite | v1.61.0 | 无 CGo SQLite | BSD-3-Clause；内嵌 SQLite 及相关通知见上游 |

直接依赖的根 LICENSE 已核对。运行时依赖与 Go 的许可证原文汇总在根目录 `THIRD_PARTY_NOTICES.txt`，容器内为 `/usr/share/doc/mail-auto/THIRD_PARTY_NOTICES.txt`。间接依赖版本见 go.mod。项目自身尚未指定开源许可证。

go-imap v2 的候选状态与 v1 的兼容性风险仍按计划逐家真实验证；0.1.0 选择稳定 v1，通过单连接串行命令与独立适配器限制耦合。ID 小扩展固定提交，不采用浮动 HEAD。

2026-10-22 的 govulncheck：Go 1.27.1 扫描发现标准库问题，已升级到修复版本 1.27.3。最终扫描当前调用符号 0 项、当前导入包 0 项已知漏洞；模块层仍报告 x/crypto/openpgp 的未维护风险 GO-2026-5932。项目不导入或调用 OpenPGP，使用的是 Argon2id，扫描退出成功。此结论仅对应当次数据库，不代表未来无漏洞；后续通过 CI 持续复查。

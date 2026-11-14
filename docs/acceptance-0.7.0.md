# 0.7.0 验证范围

日期：2026-11-14（Asia/Shanghai）。本版新增模型连接列表、按需获取模型和“检测并使用”，数据库 schema 13。

## 自动化验证

| 范围 | 对应测试 |
| --- | --- |
| 两个连接保存与切换、切回无需重新输入密钥、运行时使用对应密钥、密文按记录 AAD 隔离 | `agent.TestTwoConnectionsSwitchWithoutMixingKeys` |
| 错误密钥、超时、仅聊天（不支持工具调用）时当前配置与连接均不变 | `agent.TestFailedCheckKeepsCurrentConfiguration` |
| 已生效配置原样提交不检测、不升版本；真实更换重新检测并升版本 | `agent.TestUnchangedResubmitKeepsGeneration` |
| 并发激活、旧页面版本、连接被他处修改、检测期间网络出口变化均拒绝旧结果 | `agent.TestConcurrentChangeRejectsStaleActivation` |
| 真实切换使旧配置下的待确认方案失效 | `agent.TestSwitchInvalidatesFrozenProposals` |
| 已存密钥只用于原域名（保存与获取模型）；备用保存不影响当前配置；正在使用的连接不可删除 | `agent.TestKeyStaysWithItsOriginAndDeleteRules` |
| 旧配置迁移为连接、密钥重加密可解、运行状态不变、幂等；空配置不生成连接 | `agent.TestLegacyProviderMigratesToConnection`、`TestEmptyLegacyProviderMigratesWithoutConnection` |
| schema 12→13 保留 Provider 行与邮件数据并标记待迁移；新库无待迁移 | `store.TestSchema12UpgradeKeepsProviderAndMarksMigration`、`TestFreshDatabaseHasNoPendingConnectionMigration` |
| 模型列表分页上限、不跟随服务端 URL、状态码映射、地址/密钥校验 | `modelprovider.TestListModelsPagesWithinOriginAndSorts`、`TestListModelsBoundsAndErrors` |
| 管理 API 鉴权、CSRF、密钥脱敏、失败的中文说明、旧版本冲突 | `admin.TestModelConnectionAPI` |

另运行 `make check`（race、vet、gofmt、staticcheck）。界面检查脚本 `scripts/ui-check/model-ui-check.js`（假 API）覆盖：当前生效标识、预设填址、迟到的模型列表被丢弃、列表失败可手填、检测失败保留草稿与当前配置、切换与切回、更换域名提示重填密钥、取消修改、高级设置非默认摘要、刷新不覆盖草稿、窄屏无横向溢出。

## 验证边界

单元与界面检查使用假模型和假 API。升级演练用 0.6.1 实际二进制生成 schema 12 数据库后由 0.7.0 打开；其中“已启用且已检测”状态为按 0.6.1 摘要规则写入的模拟状态。真实模型接口的获取与切换只能在可直接解析公网地址的环境（生产部署）中验证。服务商预设只填入地址，不保证每家接口都支持工具调用。

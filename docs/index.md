# qqmail-cli 文档

qqmail-cli 是独立的第三方开源项目，与腾讯及 QQ 邮箱不存在隶属、合作或官方授权关系；项目通过用户主动开启的标准 IMAP/SMTP 服务工作。与 qmail 生态的 qmailctl 工具无任何关联。

代码版本 `0.7.0` 支持只读 IMAP、本地索引、门禁清理、白名单发送、服务端搜索分页、可恢复只读轮询与实验性 [MCP 桥](MCP_READONLY.md)，新增网络有界重试与多 IP 故障转移、可选 `auth login --verify-smtp` 与安全端口回退、单次分阶段 `doctor --smtp/--no-auth`、无头 Linux 凭据诊断与 `auth keyring-test`、只读 `config validate/policy/diff` 与未知字段防覆盖保护，以及 [无头 Linux / 云 VM 与 Muse 部署指南](CLOUD_DEPLOYMENT.md)。邮件内容是不可信数据。

从 `qqmail-cli auth login` 开始，再运行 `qqmail-cli doctor --json`。Agent 应读取 `qqmail-cli agent-info` 和 `qqmail-cli schema <command>`，先列信封，再一次批量读取需要的邮件。

## 开始使用

- [功能与设计全景](OVERVIEW.md)：一篇读懂"能做什么、怎么做到的、为什么这样设计"。
- [项目介绍](INTRODUCTION.md)：解决什么问题、适合谁、安全模型与边界。
- [完整使用手册](USER_GUIDE.md)：安装、登录、读取、检索、分类、备份、清理和发送。
- [无头 Linux / 云 VM 与 Muse 部署指南](CLOUD_DEPLOYMENT.md)：受支持网络授权、D-Bus/Keyring 配置、VM 更换恢复与排障决策树。
- [安全发送](sending.md)：白名单、MIME、SMTP 和 TTY 确认。
- [Triage 自定义规则](triage-rules.md)：TOML 正则格式与匹配顺序。

## 理解与开发

- [架构说明](ARCHITECTURE.md)：组件、数据模型、读写边界和发布结构。
- [v0.2 安全模型](v0.2-safety.md)：clean 三道门、fallback 和审计。
- [测试手册](TESTING.md)：自动化、真实只读冒烟和专用账号测试矩阵。
- [兼容性记录](compat/README.md)：带日期且脱敏的真实 QQ 服务器观察。

## 开源协作

- [贡献指南](../CONTRIBUTING.md)
- [安全策略](../SECURITY.md)
- [行为准则](../CODE_OF_CONDUCT.md)
- [变更日志](../CHANGELOG.md)

---

维护者：**司徒K** ｜ 公众号：**司徒K** ｜ 个人网站：[www.situking.com](https://www.situking.com)

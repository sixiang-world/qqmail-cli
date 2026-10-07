# AGENTS.md — qqmail-cli 编码代理指南

# AI 编程规则

1. 改动旧接口、数据结构或行为前，先确认兼容要求。只有明确不需要兼容时，才移除旧路径；不要自行删除迁移或回退方案。
2. 选择能满足当前需求的最简单实现。别为尚未出现的需求增加抽象、配置和间接层。
3. 先做出能从头到尾跑通的最小版本，再逐步添加能力。不要为了尚未完成的复杂设计拆掉现有可用功能。
4. 保持组件职责清楚，相关代码放在合适的位置。
5. 成熟且维护良好的库能降低复杂度或提高可靠性时，优先使用；没有明确理由不要重写常见功能。
6. 写新实现或加新依赖前，先检查项目已有依赖的文档、类型和现成能力。
7. 做架构选择时考虑后续维护，不用明知很快要推倒的临时方案糊弄过去。
8. 设计方案前，看看成熟产品怎样解决同类问题；适合当前需求时，沿用已经验证过的做法。

## 仓库是什么

安全优先的 **QQ 邮箱 CLI**（Go 1.25，`CGO_ENABLED=0` 纯 Go，无外部工具链）。走用户主动开启的标准 IMAP/SMTP + 16 位授权码；与腾讯无隶属。核心价值是"连上之后敢交给自动化"：凭证不落明文、读不留痕、写有门禁、删有后路、Agent 纪律内建。多服务商需求不在此仓库解决。

**主要目录**：`cmd/qqmail-cli`（入口）、`internal/cli`（cobra 命令层）、`internal/policy`（门禁与审计）、`internal/imapx`（IMAP 层，**唯一允许 import go-imap 的包**）、`internal/sendmail`（MIME 构建与发送，含 autosend 计数器）、`internal/mimeparse`（邮件解析）、`internal/cleaner` / `internal/cleanupplan`（清理与计划生成）、`internal/triage`、`internal/index`（SQLite/FTS5 本地索引）、`internal/account`（账号配置解析，`auto_send`/`send_blacklist`/`daily_auto_limit` 在此定义）、`internal/secrets`（凭证存储，不落明文）、`internal/safeio`（受限 IO）、`internal/export` / `internal/syncer` / `internal/mailmodel`（导出/同步/邮件建模）、`internal/readonlyaudit`（AST 守卫测试）、`internal/output`（人读脱敏）、`internal/errmap`（语义化退出码，唯一退出码出口）、`internal/e2e`（端到端测试）。仓库根另有 `schemas/`（内嵌 JSON Schema）、`docs/`、`skills/qqmail-cli/`（Agent 技能文件）、`scripts/`、`spikes/`（方言探针与一次性实验，不进构建）。

## 常用命令

```bash
CGO_ENABLED=0 go build -o bin/qqmail-cli ./cmd/qqmail-cli
go test ./...            # 全量测试（含只读守卫与契约测试），提交前必须全绿
go vet ./...             # 提交前必须干净
golangci-lint run
govulncheck ./...
```

单测焦点：`go test ./internal/<pkg>/ -run <TestName> -v`。Windows PowerShell 5.1 处理中文 JSON 前先设 `[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)`。

## 安全架构边界（改代码前必读，守卫测试在 CI 强制）

- **写原语圈禁**：go-imap 的写调用只允许出现在 `internal/imapx/mutate.go`；新 Mutator 方法必须同步加入 `internal/readonlyaudit/audit_test.go` 的 `mutationMethods` 白名单。命令层只能经 `internal/policy` 调写原语。
- **Reader 冻结**：`imapx.Reader` 接口方法面与 `readonly_test.go` 反射白名单不得改动。
- **禁 EXPUNGE/CLOSE**：任何情况下不得新增永久删除路径（AST 禁用表 + 协议线路断言守着）。
- **禁 bulk BODYSTRUCTURE**：`FetchEnvelopes` 不得抓取 BODYSTRUCTURE（守卫 `TestBulkFetchNeverRequestsServerParsedStructure`；QQ 畸形 BODYSTRUCTURE 会令解码器失步杀会话，见 `docs/compat/qq-20260902.md`）。信封 `has_attachments` 恒 false 是这个决策的产物，不是 bug。
- **cli 层零 go-imap import**：守卫 `TestOnlyIMAPXImportsGoIMAP` 连 `_test.go` 一起扫。
- **写门禁链**：dry-run 默认 → `--execute` → `policy.RequireMutationAllowed()` → 真实 TTY `confirmExactCount` → 内容无关审计 JSONL。没有 bypass flag。
- **readonly**：`QQMAIL_CLI_READONLY=1` 锁死全部写命令（未知取值按只读）；新写命令必须加进 `cli_test.go` 的 `TestReadonlyEnvironmentBlocksEveryMutatingCommandBeforeDial` 矩阵（readonly+`--execute`→退出码 50，无 execute→dry-run 退出 0）。
- **新叶命令**必须登记 `internal/cli/agent.go` 的 `commandCatalog()`（`TestCommandTreeMatchesDeclaredRiskCatalog` 强制）。
- **schema 注记 ↔ agent-info**：schema 里标 `UNTRUSTED` 的字段必须同步进 `agent.go` 的 `untrusted_paths`（`untrusted_paths_test.go` 交叉核对）；邮件衍生数据一律标注。
- **JSON 契约**：输出 `schema_version: "1"`，字段只增不删；新命令要有 `schemas/<command>.schema.json`。`--json` 时 stdout 必须是单个机器可读文档，诊断/人机确认走 stderr。
- **人读输出**一律过 `output.SanitizeHuman`（剥控制符/ANSI/bidi）；邮件衍生字段按不可信数据处理。
- **发送门禁**：`send_allowlist` 全员命中、单次一封、≤10 收件人、附件合计 ≤20 MiB、正文 ≤1 MiB；默认确认必须真实 TTY 键入 `SEND`。**autosend**（账号配置 `auto_send = true`）允许 allowlist 内、未命中 `send_blacklist`、当日本地计数器（默认 `daily_auto_limit = 50`，全局单文件、多账号共享，不进审计不进输出）未达上限的发送跳过 TTY；黑名单命中或计数器满仍按完整人工门禁拒绝。readonly 永远优先于 autosend。`--draft-file` 从 TOML 草稿组装发送，仍走同一门禁链。
- **退出码**：语义化映射（0/1/2/3/10/11/20/21/30/40/50/60/70），见 SKILL.md 表；`errmap` 是唯一出口。
- **测试夹具一律合成**，仓库不接收真实邮件样本。

## 外部库版本坑（已实证，勿凭记忆写 API）

- **go-imap v2.0.0-beta.8**：`imap.StoreFlagsDel`（无 StoreFlagsRemove）；`Append(mailbox string, size int64, opts)` 无 Reader 参数——`cmd.Write(raw)` → `cmd.Close()` → `cmd.Wait()` 三段式；`Rename(old, new, options)` 三参；SEARCH 日期线上格式 `2-Jan-2006` 无前导零；非 ASCII SEARCH 词走 literal 编码（无引号形态）；tagged NO/BAD 包成 `*imap.Error{Type, Code, Text}`（常量 `StatusResponseTypeNo/Bad`）。
- **go-message v0.18.2**：`InlineHeader` 没有 `Filename()`（只有 `AttachmentHeader` 有）；文件名走 `ContentDisposition()` params → `ContentType()` params["name"]。
- Windows checkout 是 autocrlf（CRLF），`gofmt -l` 有既有噪音——判断格式问题先 `gofmt -d` 看具体 diff，别被 `-l` 吓到。

## 必读文档索引

改安全相关区域前先读：`README.md`（全景与安全模型表）、`docs/ARCHITECTURE.md`、`docs/v0.2-safety.md`（清理门禁）、`docs/sending.md`（发送面）、`docs/compat/`（QQ 方言实测记录，新增方言观察按此格式落档）、`skills/qqmail-cli/SKILL.md`（Agent 纪律与退出码表）、`CHANGELOG.md`（Unreleased 分节惯例：日期分轮、Added/Fixed/Security）、`CONTRIBUTING.md`、`SECURITY.md`（安全问题私密报告，不开公开 Issue）。

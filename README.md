<div align="center">

# qqmail-cli

**敢把 QQ 邮箱交给脚本和 AI Agent 的命令行工具。**

[![CI](https://github.com/sixiang-world/qqmail-cli/actions/workflows/ci.yaml/badge.svg)](https://github.com/sixiang-world/qqmail-cli/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/sixiang-world/qqmail-cli)](https://github.com/sixiang-world/qqmail-cli/releases)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

**简体中文** · [English](README.en.md)

</div>

> [!IMPORTANT]
> **本仓库是 [situker/qqmail-cli](https://github.com/situker/qqmail-cli) 的活跃 fork。**
> 上游作者司徒K 的版本停在 `0.3.0-dev`（标签 v0.1.0）；本仓库在其基础上继续开发并发布
> **v0.4.0**——服务端关键词检索、星标/回收站/文件夹管理、HTML 与内嵌图发送、服务器草稿等
> 12 项功能补全（见 [v0.4.0 亮点](#v040-做了什么)）。上游署名按 [NOTICE](NOTICE) 完整保留，
> 项目原设计原则与全部安全门禁一脉相承。
>
> qqmail-cli 是独立的第三方开源项目，与腾讯及 QQ 邮箱不存在隶属、合作或官方授权关系；
> 项目通过用户主动开启的标准 IMAP/SMTP 服务工作。与 qmail 生态的 qmailctl 工具无任何关联。

qqmail-cli 是一个安全优先的 QQ 邮箱 CLI：读信、检索、分类、备份、带门禁的清理和白名单发送，全部走稳定 JSON 契约。它解决的不是"能不能连上邮箱"，而是连上之后的那些事——凭证会不会泄露、Agent 会不会误删邮件、邮件正文会不会反过来指挥 Agent。

```console
$ qqmail-cli envelope list --unread --limit 2 --json
{
  "schema_version": "1",
  "command": "envelope.list",
  "ok": true,
  "data": {
    "envelopes": [
      {
        "id": "m1_eyJmIjoiSU5CT1giLCJ2IjoxNDI1LCJ1Ijo4MzQ3fQ",
        "uid": 8347,
        "uidvalidity": 1425,
        "folder": "INBOX",
        "subject": "9 月对账单",
        "from": [{ "email": "billing@example.com" }],
        "to": [{ "email": "me@qq.com" }],
        "date": "2026-08-30T09:12:00+08:00",
        "internal_date": "2026-08-30T09:12:00+08:00",
        "size_bytes": 18204,
        "flags": [],
        "has_attachments": false
      },
      { "…": "第二封信封结构相同，略" }
    ],
    "page": { "next_before_uid": 8346 }
  },
  "error": null,
  "warnings": [],
  "meta": { "account": "personal", "duration_ms": 1, "truncated": false, "search_mode": "server" }
}

$ qqmail-cli message show "m1_eyJmIjoiSU5CT1giLCJ2IjoxNDI1LCJ1Ijo4MzQ3fQ" \
                        "m1_eyJmIjoiSU5CT1giLCJ2IjoxNDI1LCJ1Ijo4MzQ2fQ" --json
```

（示例由真实运行输出生成并通过项目自身 schema 校验；实际输出为单行紧凑 JSON，此处为阅读排版。）一次列信封、一次批量读全文，`id` 是不透明 token，原样复制即可；读取全程 `EXAMINE` + `BODY.PEEK`——服务器上的未读状态碰都不碰。

## 目录

[它能帮你做什么](#它能帮你做什么) · [v0.4.0 做了什么](#v040-做了什么) · [为什么是 qqmail-cli](#为什么是-qqmail-cli) · [什么时候不该用它](#什么时候不该用它) · [安装](#安装) · [五分钟上手](#五分钟上手) · [常用工作流](#常用工作流) · [给 AI Agent 用](#给-ai-agent-用) · [安全模型](#安全模型) · [FAQ](#faq) · [项目状态](#项目状态) · [参与贡献](#参与贡献) · [致谢与许可](#致谢与许可)

## 它能帮你做什么

- **整理邮箱** —— 把成千上万封邮件按发件人、类别、时间归堆，一眼看清谁在给你发垃圾。
- **清理垃圾邮件** —— 规则挑出营销和通知邮件，先本地全量备份、你亲手确认，再移入回收站；删错了 `restore` 一条命令整单找回，本地备份永久兜底。
- **备份存档** —— 把邮件导出成本地 `.eml` 文件，带哈希校验，随时可验证完整性。
- **本地与服务端检索** —— 本地 FTS5 全文索引（含中文）比网页版快且不联网；没建索引的邮箱让服务器直接搜（`search --server`，按正文匹配）。
- **交给 AI Agent** —— 让 AI 帮你读信、提行动项、起草回复；但读是读、删是删、发是发，删除和发送权牢牢卡在你的人工确认里。
- **安全发送** —— 发信走收件人白名单 + 人工键入确认，每次一封；HTML 正文、内嵌图（`--attach-inline` + `cid:`）与服务器草稿（`--save-draft`）走同一套门禁，星标管理、移入回收站、文件夹整理也都有 dry-run 与人工确认兜底。

## v0.4.0 做了什么

这是本 fork 相对上游的主要增量（上游停在 0.3.0-dev 的读取内核 + 门禁清理 + 白名单发送）：

- **服务端关键词检索** —— `search --server` 按正文检索，无需先建索引；检索方言经真实服务器实测落档（QQ 静默忽略 `TEXT`，定案 `BODY`，见 [docs/compat/qq-20261006.md](docs/compat/qq-20261006.md)）。
- **信封过滤补全** —— `envelope list --before/--to`，与既有 `--since/--from/--subject` 组合成时间窗。
- **写动词补全** —— `message mark-unread`、`message flag`（星标加/除）、`message trash`（单封移入回收站）、`folder create/rename`，全部走 dry-run → `--execute` → 真实 TTY 确认的门禁链。
- **发送面补全** —— `--body-format html`（multipart/alternative，预览展示纯文本降级与转义源码摘要）、`--attach-inline`（内嵌图 + 自动 `cid:` 引用）、`reply --reply-all`。
- **服务器草稿** —— `--save-draft` 把 send/reply/forward 的完成动作从 SMTP 提交换成存草稿箱，不经发送门禁、但仍需人工确认。
- **内嵌图读取闭环** —— `multipart/related` 邮件的 CID 图片可列出、可下载、转发不丢图。
- **可验证的诚实** —— QQ 搜索方言与草稿箱位置来自只读探针实测（[spikes/imap-dialect-probe](spikes/imap-dialect-probe/)），结论落档 `docs/compat/`；文件夹名编码修正为 RFC 3501 Modified UTF-7 并与 go-imap 序列化器做了差分验证。

## 为什么是 qqmail-cli

把邮箱接进自动化，市面上不缺"能收发"的库和工具，缺的是从一开始就把下面这些事认真当回事的工具：

- **凭证不落明文** —— 16 位授权码只进操作系统凭据管理器（Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）。不存在授权码命令行参数，配置文件、日志、错误输出、panic 栈全部脱敏。
- **读不留痕** —— 只读路径永远 `EXAMINE` + `BODY.PEEK`，看邮件不会把它标成已读。
- **邮件是数据，不是指令** —— 主题、正文、发件人、附件名全部按不可信输入处理：JSON 逐字段标注 `UNTRUSTED`，人读界面剥离 ANSI 转义、控制符和 bidi 覆盖符——一封精心构造的邮件伪造不了你确认界面上的任何一行。
- **写有门禁** —— 一切服务器写操作默认 dry-run；真实执行要过策略层、真实 TTY 键入确认、全程审计，且**不存在任何 bypass flag**（这一条本身有测试守着）。
- **删有后路** —— 清理前必须有哈希校验 + HMAC 签名的本地备份，执行时逐封核对服务器真相；星标邮件受绝对保护，没有开关能放行；只移入回收站、没有永久删除命令，协议层守卫连 `EXPUNGE`/`CLOSE` 的下发路径都封死；后悔了还有 `restore` 整单找回。
- **中文过硬** —— GB2312/GBK/GB18030 主题与附件名、Modified UTF-7 中文文件夹、双解析器兜底畸形 MIME、FTS5 中文 bigram 本地全文检索，配套合成语料回归测试。
- **Agent 原生** —— 版本化 JSON 包络、`agent-info` 能力自发现、语义化退出码（可重试与否机器可判）、随仓分发 Agent 技能文件，`QQMAIL_CLI_READONLY=1` 一个环境变量把整个写面锁死。
- **本地优先，零遥测** —— 不联网上报任何数据，本地索引默认不存正文，`cache clear` 删的是整个库文件。

这些不是文档承诺：只读边界、写路径圈禁、无 bypass、禁 EXPUNGE 全部写成了跑在 CI 里的守卫测试（接口反射白名单 + AST 扫描 + 协议线路断言）。

## 什么时候不该用它

坦白比全能更省你的时间：

- **你用的不是 QQ/Foxmail 邮箱。** 本项目的定位就是把 QQ 邮箱单点做透；多服务商需求请看 [himalaya](https://github.com/pimalaya/himalaya)。
- **你要实时新邮件推送。** 真实观察记录显示 QQ 的 IMAP IDLE 通知不稳定，所以 `watch` 是诚实的轮询（默认 60 秒），不许诺实时性。
- **你要网页版才有的功能。** 通讯录、日历、超大附件中转站不在标准 IMAP/SMTP 里，本项目也绝不逆向网页接口。
- **你要群发营销邮件。** 发送面按"单次一封 + 收件人白名单 + 人工确认"设计，且刻意不打算改变。
- **你想绕过 QQ 的频率限制。** 这里的设计方向恰恰相反：批量动词减少登录次数、限流时明确退出并建议等待，不做任何形式的对抗。

## 安装

### 方式一：下载预编译二进制

从 [GitHub Releases](https://github.com/sixiang-world/qqmail-cli/releases) 下载对应平台的归档（v0.4.0 起）：

| 平台 | 架构 | 说明 |
|---|---|---|
| Windows | amd64 / arm64 | 解压得 `qqmail-cli.exe`，放进 PATH 即可 |
| macOS | Intel / Apple Silicon | `chmod +x` 后使用 |
| Linux | amd64 / arm64 | 同上 |

下载后先核对校验和（Release 附 `checksums.txt`，并带构建来源 attestation，可用 `gh attestation verify` 验证）：

```powershell
Get-FileHash .\qqmail-cli.exe -Algorithm SHA256
```

Windows 首次运行未签名程序会弹 SmartScreen 提示，属预期行为：核对过校验和后点"更多信息 → 仍要运行"。

### 方式二：从源码构建

需要 Go 1.25+，纯 Go 构建，无 CGO、无外部工具链：

```bash
git clone https://github.com/sixiang-world/qqmail-cli.git
cd qqmail-cli
CGO_ENABLED=0 go build -o bin/qqmail-cli ./cmd/qqmail-cli
```

Windows PowerShell：

```powershell
git clone https://github.com/sixiang-world/qqmail-cli.git
cd qqmail-cli
$env:CGO_ENABLED = "0"
go build -o .\bin\qqmail-cli.exe .\cmd\qqmail-cli
.\bin\qqmail-cli.exe version --json
```

### Windows PowerShell 5.1 中文设置

处理中文 JSON 前建议先执行：

```powershell
$utf8 = New-Object System.Text.UTF8Encoding($false)
[Console]::OutputEncoding = $utf8
$OutputEncoding = $utf8
```

## 五分钟上手

```text
1. QQ 邮箱网页端 → 设置 → 账号与安全 → 开启 IMAP/SMTP 服务，生成 16 位授权码
   （官方指引：https://service.mail.qq.com/detail/0/1087）

2. qqmail-cli auth login --email your-account@qq.com
   授权码隐藏输入，验证连接成功后才写入系统凭据管理器

3. qqmail-cli doctor --json
   一条命令诊断配置、凭证、TLS、登录与服务器能力

4. qqmail-cli envelope list --unread --limit 20 --json

5. qqmail-cli message show <id> --json
   id 从上一步的输出里原样复制，不要手拼
```

授权码不是 QQ 密码——新手十有八九栽在这，`doctor` 会帮你诊断。改 QQ 密码会让所有授权码立即失效，届时重新 `auth login` 即可（已有配置不会丢）。

## 常用工作流

### 本地检索与邮箱清理

```text
qqmail-cli sync --json                                    # 增量同步元数据进本地 SQLite
qqmail-cli search "发票" --local --json                    # FTS5 中文全文检索
qqmail-cli triage analyze --json                          # 规则归堆：营销/通知/验证码…
qqmail-cli triage plan --output plan.json --markdown plan.md
qqmail-cli backup --plan plan.json --output backup        # 计划内邮件全量 .eml 备份 + 校验
qqmail-cli clean --plan plan.json                         # dry-run：只报告，不动服务器
qqmail-cli clean --plan plan.json --execute               # 三道门（备份验证/服务器核对/人工确认）后移入回收站
qqmail-cli restore --plan plan.json                       # 后悔药：从回收站整单找回（dry-run）
```

分类是本地确定性规则，CLI 永远不调用任何 AI 模型。计划默认只圈营销、机器通知、社交通知三类，只扫收件箱、只收 30 天前的邮件，星标邮件绝对排除——被排除的数量和原因都会列给你审。清理执行默认单批上限 500 封（提高须人工显式传 `--batch-limit`），每一封都要求本地备份验证 + 服务器逐封核对通过。

### 安全发送

先在账号配置里设置收件人白名单（空白名单 = 拒发一切）：

```toml
[accounts.personal]
email = "your-account@qq.com"
send_allowlist = ["you@example.com", "*@your-company.example"]
```

```text
qqmail-cli send --to you@example.com --subject "主题" --body "正文"   # dry-run：完整展示信封
qqmail-cli reply <id> --reply-all --body "回复内容"                    # 自动带线程头；--reply-all 并入原收件人
qqmail-cli forward <id> --to you@example.com --body "转发说明"         # 自动携带原邮件附件
```

真实发送必须追加 `--execute`、全部收件人命中白名单、由人在真实 TTY 键入 `SEND`。每次调用最多一封。

## 给 AI Agent 用

给 Agent 会话的第一行配置：

```powershell
$env:QQMAIL_CLI_READONLY = "1"    # 锁死一切写操作与发送；未知取值一律按只读处理
```

Agent 集成三件套：

- `qqmail-cli agent-info` —— 机器可读的能力清单：全部命令与风险级别（read/mutate/send）、不可信字段路径、当前只读状态。
- `qqmail-cli schema <command>` —— 输出内嵌 JSON Schema，消费任何新形状前先自校验。
- [`skills/qqmail-cli/SKILL.md`](skills/qqmail-cli/SKILL.md) —— 随仓分发的 Agent 技能文件，装完 CLI 即获得完整调用纪律（Claude Code 等 harness 直接可用）。

关键纪律：

- **批量读**：先 `envelope list` 一次，再把所有要读的 id 交给**一次** `message show`——每次 CLI 调用就是一次 IMAP 登录，高频登录会触发 QQ 风控。
- **可重试才重试**：`error.retryable` 为 true 才可重试（指数退避，至多两次）；退出码 30（限流）立即停手等 10–15 分钟；50（policy_denied）代表安全门禁在工作，需要的是人而不是重试。
- **v0.4 起 Agent 能经同一套门禁做更多**：星标管理（`message flag`）、移入回收站（`message trash`）、文件夹整理（`folder create`/`rename`）、服务器端检索（`search --server`）、存服务器草稿（`--save-draft`）、内嵌图发送（`--attach-inline` + `cid:`）——写命令默认 dry-run，`agent-info` 与 `schema <command>` 随时可查风险级别与输出契约。
- **发送闭环（需人显式开启）**：账号配置开启 `auto_send` 后，Agent 可用 `send --draft-file` 完成 allowlist 内、黑名单之外的自动发送（受每日限额约束）；收件人命中黑名单即回落 TTY 门禁，readonly 永远优先，allowlist/blacklist 没有 CLI 改写路径。

## 安全模型

| 层 | 机制 | 验证方式 |
|---|---|---|
| 凭证 | 系统凭据管理器；env 注入需显式 `--auth-code-env`；全通道脱敏 | 夹具授权码全输出扫描测试，含 panic 路径 |
| 只读 | EXAMINE + PEEK；读接口白名单 | 接口反射测试 + 内存 IMAP 服务器行为断言 |
| 写边界 | go-imap 写方法圈禁在单一文件、仅策略层可调 | go/ast 静态扫描跑在 CI（无条件防护） |
| 不可删 | 无永久删除命令；EXPUNGE/CLOSE 全域禁止 | AST 禁用表 + 协议线路断言 |
| 清理门禁 | 备份 HMAC + 本地哈希 + 服务器逐封核对 + TTY 键入数量 | 门禁各失败分支表驱动测试 + 端到端测试 |
| 发送门禁 | 白名单全员命中 + dry-run 默认 + TTY 确认（键入 `send`，大小写不限；账号显式开启 `auto_send` 且收件人不在 `send_blacklist` 时，在每日限额内跳过 TTY 自动完成）+ 单发一封 | 本地 TLS SMTP fixture 回归 |

深入阅读：[清理安全模型](docs/v0.2-safety.md) · [安全发送](docs/sending.md) · [安全策略](SECURITY.md)

## FAQ

<details>
<summary><b>授权码是 QQ 密码吗？</b></summary>

不是。授权码是 QQ 邮箱专为第三方客户端生成的 16 位凭证，在网页端"设置 → 账号与安全"里开启 IMAP/SMTP 服务后生成。qqmail-cli 只认授权码，永远不要在任何地方输入 QQ 密码。注意：修改 QQ 密码会让全部授权码立即失效。
</details>

<details>
<summary><b>我的邮件会被上传到哪里吗？</b></summary>

不会。qqmail-cli 零遥测、本地优先：唯一的网络连接就是你的机器与 QQ 服务器之间的 TLS 直连。本地索引默认只存信封元数据，正文落盘需要显式 `--cache-bodies`，`cache clear` 删除整个库文件。
</details>

<details>
<summary><b>Agent 会不会误删我的邮件？</b></summary>

这是整个项目的设计原点。五层答案：`QQMAIL_CLI_READONLY=1` 从源头锁死写面；清理计划默认只圈明确的垃圾类别且星标邮件无条件排除；执行前有备份门禁与服务器逐封核对；确认要求真实 TTY 人工键入数量，Agent 的管道喂不进去；就算全过了，动作也只是移入回收站——`restore` 能整单找回，本地还有 .eml 备份。永久删除命令在这个项目里不存在。
</details>

<details>
<summary><b>为什么没有实时新邮件推送？</b></summary>

带日期的真实服务器观察显示 QQ 的 IMAP IDLE 通知不稳定（详见 <a href="docs/compat/README.md">兼容性记录</a>）。与其给一个不可靠的"实时"，`watch --jsonl` 选择了诚实的轮询，事件流带 UID 水位去重，不会重复播报。
</details>

<details>
<summary><b>会触发 QQ 的风控或限流吗？</b></summary>

腾讯官方确认存在登录频率与连接数限制且数值保密。qqmail-cli 的应对是顺应而非对抗：单次调用单连接、批量动词减少登录、限流时给出明确的退出码 30 和等待建议、认证失败绝不自动重试。SKILL.md 把这套纪律直接教给 Agent。
</details>

<details>
<summary><b>Windows 提示"未知发布者"怎么办？</b></summary>

二进制尚未购买代码签名证书，SmartScreen 弹窗属预期。请先用 <code>checksums.txt</code> 核对哈希（Release 同时提供构建来源 attestation），确认无误后选择"更多信息 → 仍要运行"。
</details>

<details>
<summary><b>支持 163、Gmail 或企业邮箱吗？</b></summary>

不支持，也不打算支持。qqmail-cli 的价值主张就是把 QQ 邮箱一家做透：官方口径逐条查证、服务器方言逐条实测、中文场景逐条测试。多服务商需求推荐 <a href="https://github.com/pimalaya/himalaya">himalaya</a>。
</details>

<details>
<summary><b>这个 fork 和上游是什么关系？</b></summary>

本仓库 fork 自 <a href="https://github.com/situker/qqmail-cli">situker/qqmail-cli</a>。上游作者司徒K 完成了 v0.1–v0.3 的全部设计（只读内核、门禁清理、白名单发送）与整个安全架构，这份基础与设计原则在 fork 中原样保留并持续遵守。上游暂停在 <code>0.3.0-dev</code> 后，本 fork 继续开发并发布 v0.4.0（能力清单见<a href="#v040-做了什么">上文</a>）。安全模型与门禁纪律没有放松，新增功能全部通过同一套守卫测试。随时欢迎上游合并本 fork 的改动。
</details>

## 同类项目

- [himalaya](https://github.com/pimalaya/himalaya) —— Rust 生态成熟的多后端邮件 CLI，名词-动词命令树的代表作（本项目的命令语法向它看齐）。多服务商场景选它。
- 通用 IMAP 库/工具能解决"连上"，qqmail-cli 解决的是"连上之后敢不敢交给自动化"——两类工具是互补而非替代。

## 项目状态

代码版本 `0.4.0`（[Release](https://github.com/sixiang-world/qqmail-cli/releases/tag/v0.4.0)），覆盖只读内核（v0.1）、本地索引与门禁清理（v0.2）、白名单发送（v0.3）、服务端检索与写动词/发送面补全（v0.4）四个阶段的能力面。截至 2026-10-07：全量单元与集成测试、静态守卫、漏洞扫描、PowerShell 5.1 冒烟、六平台构建全部通过；真实 QQ 服务器的只读行为观察（搜索方言定案、草稿箱解析、畸形 MIME 记录）见[兼容性记录](docs/compat/README.md)。发布节奏见 [CHANGELOG](CHANGELOG.md)。

## 参与贡献

```text
go test ./...          # 全量测试（含只读守卫与契约测试）
go vet ./...
golangci-lint run
govulncheck ./...
```

JSON 输出是稳定契约（`schema_version: "1"`，字段只增不删）；人读文本不是契约。提交前请读 [CONTRIBUTING.md](CONTRIBUTING.md)；安全问题请按 [SECURITY.md](SECURITY.md) 私密报告，不要开公开 Issue。测试夹具必须合成，仓库不接收任何真实邮件样本。Issue 与 PR 请提到本仓库（[sixiang-world/qqmail-cli](https://github.com/sixiang-world/qqmail-cli)）。

## 致谢与许可

- **项目原作者与上游维护者：[司徒K](https://github.com/situker)（[situking.com](https://www.situking.com)）** —— 本仓库 fork 自 [situker/qqmail-cli](https://github.com/situker/qqmail-cli)，v0.1–v0.3 的全部设计、安全架构与门禁体系出自上游，fork 中原样保留并持续遵守。想聊 AI 落地与 skill/agent 工程，上游作者的[公众号](https://www.situking.com)与[个人网站](https://www.situking.com)仍在更新。
- [Apache License 2.0](LICENSE) · 版权与作者署名见 [NOTICE](NOTICE)（转发与二次分发须保留）。

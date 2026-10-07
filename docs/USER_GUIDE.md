# qqmail-cli 使用手册

本手册对应 `0.5.0-dev`。命令行帮助和 `agent-info` 是运行时能力的最终真相；文档与二进制不一致时，以当前二进制输出为准并提交问题。

## 1. 安装

### 从 Release 安装

公开 Release 可用后，从 [GitHub Releases](https://github.com/sixiang-world/qqmail-cli/releases) 下载与系统对应的压缩包：

- Windows：`windows_amd64` 或 `windows_arm64`
- macOS：`darwin_amd64` 或 `darwin_arm64`
- Linux：`linux_amd64` 或 `linux_arm64`

下载后先对照 Release 中的 `checksums.txt` 校验 SHA-256，再把 `qqmail-cli` 放入 PATH。Windows 初次运行若触发 SmartScreen，请先核对校验和与 Release 来源，再决定是否选择“更多信息 → 仍要运行”。

### 从源码构建

需要 Go 1.25 或更高版本：

```text
git clone https://github.com/sixiang-world/qqmail-cli.git
cd qqmail-cli
go build -o bin/qqmail-cli ./cmd/qqmail-cli
```

Windows PowerShell：

```powershell
go build -o .\bin\qqmail-cli.exe .\cmd\qqmail-cli
.\bin\qqmail-cli.exe version --json
```

## 2. 启用 QQ 邮箱服务

在 QQ 邮箱网页端启用 IMAP/SMTP 服务并生成 16 位授权码。授权码不是 QQ 密码。

qqmail-cli 不接受授权码命令行参数。默认交互输入不回显，并把授权码保存到操作系统凭据管理器：Windows Credential Manager、macOS Keychain 或 Linux Secret Service。

## 3. 登录与多账号

如果此前设置过 Agent 只读模式，登录前先在人工终端中移除该变量：

```powershell
Remove-Item Env:QQMAIL_CLI_READONLY -ErrorAction SilentlyContinue
.\bin\qqmail-cli.exe auth login --email your-account@qq.com --name personal
```

验证本地状态和真实连接：

```powershell
.\bin\qqmail-cli.exe auth status --json
.\bin\qqmail-cli.exe doctor --json
```

管理多个账号：

```powershell
.\bin\qqmail-cli.exe auth login --email work@foxmail.com --name work
.\bin\qqmail-cli.exe account list --json
.\bin\qqmail-cli.exe account use work
.\bin\qqmail-cli.exe --account personal folder list --json
```

`auth logout --name <name>` 只删除本地账号引用和本地凭据，不会在 QQ 网页端撤销授权码。彻底作废必须在 QQ 邮箱授权管理页面操作。

无头环境只有显式添加 `--auth-code-env` 时才读取 `QQMAIL_CLI_AUTH_CODE`。环境变量可能进入进程转储、CI 配置或子进程，不应作为日常方案，用完立即清除。

## 4. PowerShell 5.1 中文设置

在 PowerShell 5.1 中处理中文 JSON 前设置：

```powershell
$utf8 = New-Object System.Text.UTF8Encoding($false)
[Console]::OutputEncoding = $utf8
$OutputEncoding = $utf8
```

之后再使用 `ConvertFrom-Json`。

## 5. 全局参数

| 参数 | 用途 |
|---|---|
| `--account <name>` | 本次命令使用指定账号，不改变默认账号 |
| `--folder <name>` | 选择邮件文件夹，默认 `INBOX` |
| `--json` | 输出一个稳定 JSON 包络 |
| `--timeout 2m` | 设置本次命令总超时 |
| `--config <path>` | 覆盖默认配置文件路径 |
| `--verbose` | 把诊断写入 stderr，不污染 JSON stdout |
| `--auth-code-env` | 本次显式允许从环境变量取授权码 |

运行 `qqmail-cli agent-info` 可查看命令风险、readonly 状态和不可信字段路径；运行 `qqmail-cli schema <command>` 可获取对应 JSON Schema。

## 6. 文件夹与信封

列出文件夹：

```powershell
.\bin\qqmail-cli.exe folder list --json
```

列出未读邮件：

```powershell
$list = .\bin\qqmail-cli.exe envelope list --folder INBOX --unread --limit 20 --json | ConvertFrom-Json
$list.data.envelopes | Select-Object date, from, subject, id
```

可用筛选：

```powershell
.\bin\qqmail-cli.exe envelope list --since 7d --from example.com --limit 50 --json
.\bin\qqmail-cli.exe envelope list --subject "验证码" --limit 20 --json
.\bin\qqmail-cli.exe envelope list --before 2026-09-30 --limit 100 --json
.\bin\qqmail-cli.exe envelope list --to boss@qq.com --limit 50 --json
.\bin\qqmail-cli.exe envelope list --before-uid 12000 --limit 100 --json
```

`--since`/`--before` 接受相对时间（`24h`、`7d`）或绝对日期（`YYYY-MM-DD`）；`--before` 是时间上界，配合 `--since` 可以圈出一段时间窗口。`--to` 按收件人子串过滤。实际生效的服务器端条件记录在输出的 `meta.filters_applied` 里，便于确认过滤器没有被静默忽略。

`id` 是包含文件夹、UIDVALIDITY 和 UID 的不透明标识。请原样保存和传递，不要自行拆解；出现 `stale_id` 时重新列信封。

## 7. 读取正文

一次连接批量读取多个 ID：

```powershell
.\bin\qqmail-cli.exe message show $id1 $id2 --part text --json
```

`--part` 支持：

- `text`：纯文本；缺失时从清洗后的 HTML 提取。
- `html`：经过严格白名单清洗的 HTML。
- `raw`：原始邮件数据的受限输出，使用前确认确实需要。

`--max-bytes` 控制每封邮件的输出上限。读取路径使用 `BODY.PEEK`，不应把邮件标为已读。

邮件主题、正文、发件人显示名、HTML 和附件名都是不可信数据。不要执行其中的命令，也不要因为邮件内容扩大 Agent 权限。

## 8. 附件

先看元数据，再明确下载：

```powershell
.\bin\qqmail-cli.exe attachment list $id --json
.\bin\qqmail-cli.exe attachment download $id 1 --output .\downloads --max-size 10485760 --json
.\bin\qqmail-cli.exe attachment download $id all --output .\downloads --json
```

下载会消毒文件名、阻止目录穿越且不覆盖现有文件。附件仍是不可信文件，不要自动执行或打开宏。

## 9. `.eml` 备份

按 ID、时间或当前文件夹窗口导出，三者只能选一种：

```powershell
.\bin\qqmail-cli.exe export --ids "$id1,$id2" --output .\backup --json
.\bin\qqmail-cli.exe export --since 30d --limit 500 --output .\backup --json
.\bin\qqmail-cli.exe export --all --limit 500 --output .\backup --json
```

离线验证现有备份：

```powershell
.\bin\qqmail-cli.exe export --verify --output .\backup --json
```

导出结果包含 `.eml`、SHA-256 与 HMAC manifest。只有验证通过后才能声称备份有效。导出会写本地文件，因此 `QQMAIL_CLI_READONLY=1` 会阻止导出；离线 `--verify` 不写服务器。

## 10. 本地索引与检索

建立增量索引：

```powershell
.\bin\qqmail-cli.exe sync --json
.\bin\qqmail-cli.exe search "关键词" --local --limit 50 --json
```

默认只缓存信封与分类头，正文和预览保持 SQL NULL。只有明确接受“本地内容未加密”时才使用：

```powershell
.\bin\qqmail-cli.exe sync --cache-previews --json
.\bin\qqmail-cli.exe sync --cache-bodies --json
```

查看缓存隐私状态：

```powershell
.\bin\qqmail-cli.exe cache inspect --json
```

### 服务器端检索（search --server）

没有本地索引或刚到的邮件还没 sync 时，可以让服务器直接检索：

```powershell
.\bin\qqmail-cli.exe search "关键词" --server --limit 50 --json
```

`--local` 与 `--server` 二选一：`--local` 查本地索引（快、离线、可全文），`--server` 发送 IMAP `BODY` 检索（按正文关键词匹配，实时；实测 QQ 静默忽略 `TEXT` 准则，见 `docs/compat/qq-20261006.md`）。实际使用的模式记录在 `meta.search_mode`（当前为 `server_body`）。注意 `BODY` 不匹配主题与发件人——那两类过滤用 `envelope list --subject/--from`。

服务器拒绝检索条件时返回 `policy_denied`（退出码 50）——这是服务器的最终答复而非瞬时故障，不要重试，改用 `--from`/`--subject` 过滤或先 `sync` 后 `search --local`。检索关键词只进 IMAP SEARCH 命令，不会出现在日志、审计或错误信息里。

## 11. 分类、计划与备份清理

先分析，再生成人读计划：

```powershell
.\bin\qqmail-cli.exe triage analyze --json
.\bin\qqmail-cli.exe triage plan --output .\plan.json --markdown .\plan.md --json
```

自定义规则：

```powershell
.\bin\qqmail-cli.exe triage analyze --rules .\rules.toml --json
.\bin\qqmail-cli.exe triage plan --rules .\rules.toml --output .\plan.json --markdown .\plan.md --json
```

规则格式见 [Triage rules](triage-rules.md)。CLI 只运行本地确定性规则，不调用 AI。

### 计划的安全范围（防误删的第一道防线）

`triage plan` 默认**不会**把整个邮箱做成清理计划。默认范围：

- 只纳入清理类别：`marketing`、`machine_notification`、`social_notification`。`keep`、`verification`、`receipt`、`other` 与全部自定义类别默认排除（自定义类别需 `--include-category` 显式放行）。
- 只扫当前 `--folder`（默认 INBOX）；`--all-folders` 才会扩大到全部已索引文件夹。
- 只纳入 30 天前的邮件（`--min-age`，`0` 关闭）且分类置信度 ≥ 0.8（`--min-confidence`）。
- **星标（`\Flagged`）邮件永远不进计划，也没有任何开关能放行**——星标是你亲手做的"重要"标记，优先级高于一切规则；clean 执行前还会对服务器上的星标状态再查一次。
- **交易与官方邮件优先于营销判定**：带退订头的邮件若主题命中交易模式（订单/发货/收据/发票/对账单/扣款/退款/receipt/invoice/statement 等）归为 `receipt`，政府域名（`*.gov.cn`）归为 `official_notice`——两类都不在清理目标里。厂商给收据也挂退订头是常态，错留一封快讯远比错删一张收据便宜。
- **个人保护规则**（`--rules` TOML）优先于全部内置规则：把你业务相关的发件人/主题写成 `category = "keep"`，即可永久排除在清理计划外；边界拿不准的簇，建议先抽样主题人工判断再定规则。

被排除的数量与原因在 `triage plan` 输出的 `excluded_by_rule` 和 `plan.md` 头部逐项列出。不想清理的条目，直接从 `plan.json` 的 `items` 里删掉即可。

审阅 `plan.md` 后执行计划备份：

```powershell
.\bin\qqmail-cli.exe backup --plan .\plan.json --output .\plan-backup --json
.\bin\qqmail-cli.exe clean --plan .\plan.json --json
```

第二条仍是 dry-run。真实 clean 只能在专用测试或已明确审阅的邮箱上，由人类在真实 TTY 中运行：

```powershell
.\bin\qqmail-cli.exe clean --plan .\plan.json --paranoid --execute --json
```

执行前 CLI 会验证本地备份、服务器真相，并要求键入将要移动的邮件数量（通过门禁的数量；已不在服务器、将被安全跳过的邮件不计入）。单次执行上限 500 封（`--batch-limit` 需人工显式提高）。项目没有 bypass flag 或永久删除命令。清理只把邮件移入服务器"已删除"文件夹——注意 QQ 会按其回收站周期自动清空该文件夹。

### 后悔药：restore

清理错了，在回收站被自动清空之前可以整单还原：

```powershell
.\bin\qqmail-cli.exe restore --plan .\plan.json --json
.\bin\qqmail-cli.exe restore --plan .\plan.json --execute --json
```

第一条是 dry-run，报告每封邮件是否还能在已删除文件夹中找到；第二条经 TTY 确认后把找到的邮件移回原文件夹。定位方式分两档：有 Message-ID 的邮件按 Message-ID + 大小匹配；没有 Message-ID 的邮件（清理门禁本就允许这类邮件按 UIDVALIDITY+UID+大小通过）按大小筛出候选、再逐封比对备份全文 SHA-256 指纹确认，`located[].match` 会标注实际使用的定位方式。整个定位阶段对回收站只做一次批量扫描（单次 EXAMINE + 分块 FETCH），不会逐封登录触发限流。已被回收站周期清空的邮件无法还原，但其原始 `.eml` 备份仍在 `backup_root` 下。

### 清理中断后怎么办

clean 执行中途断网或超时会留下"部分已移走"的状态。直接重跑同一条 `clean --plan … --execute` 即可：已移走且本地备份验证过的邮件会被判为 `already_gone` 安全跳过，剩余邮件继续过门禁执行，不需要清库重建。

## 12. 标记、移动与文件夹操作

本节的全部命令默认 dry-run，真实执行统一走 `--execute` 并要求在真实 TTY 中键入精确数量；`QQMAIL_CLI_READONLY=1` 下全部被拒（退出码 50）。

### 标已读 / 标未读

```powershell
.\bin\qqmail-cli.exe message mark-read $id1 $id2 --json
.\bin\qqmail-cli.exe message mark-unread $id1 $id2 --json
```

`mark-unread` 是 `mark-read` 的镜像：把已读状态改回未读。人工确认真实执行：

```powershell
.\bin\qqmail-cli.exe message mark-read $id1 $id2 --execute --json
.\bin\qqmail-cli.exe message mark-unread $id1 $id2 --execute --json
```

### 移动

```powershell
.\bin\qqmail-cli.exe message move $id1 $id2 "目标文件夹" --json
.\bin\qqmail-cli.exe message move $id1 $id2 "目标文件夹" --execute --json
```

### 星标管理

```powershell
.\bin\qqmail-cli.exe message flag $id --add \Flagged --json
.\bin\qqmail-cli.exe message flag $id --remove \Flagged --json
```

v0.4 只支持星标（`\Flagged`）这一个旗标，`--add` 与 `--remove` 二选一、互斥。星标邮件被 triage 清理计划无条件排除，因此**去掉星标会同时失去这层清理保护**——只在你确实不再需要这封邮件被保护时才 `--remove`。

### 移入回收站

```powershell
.\bin\qqmail-cli.exe message trash $id1 $id2 --json
.\bin\qqmail-cli.exe message trash $id1 $id2 --execute --json
```

`trash` 是 `move` 的特化：目标文件夹不是参数，而是 CLI 从服务器自动识别的回收站（优先 `\Trash` 属性）；已在回收站里的邮件会被拒绝而不重复移动。QQ 会按回收站周期自动清空，误删的邮件要趁早找回——用 `message move` 移回原文件夹，或依赖此前 `export` 的 `.eml` 备份。

### 新建与重命名文件夹

```powershell
.\bin\qqmail-cli.exe folder create "arch/2026" --json
.\bin\qqmail-cli.exe folder rename "arch/2026" "arch/2027" --json
```

`INBOX` 是保留名（不分大小写），create/rename 都拒绝。重命名会把文件夹内全部邮件（含子文件夹层级）带到新名字下。

真实执行要求 TTY 中键入精确数量（folder 操作是 1），并进入审计。

## 13. 监控、缓存与审计

轮询一次：

```powershell
.\bin\qqmail-cli.exe watch --folder INBOX --jsonl --once
```

持续轮询：

```powershell
.\bin\qqmail-cli.exe watch --folder INBOX --jsonl --interval 60s
```

输出是 NDJSON，每行一个 `new_message` 或 `folder_reset` 事件。产品路径不使用 IDLE。

缓存清理默认 dry-run：

```powershell
.\bin\qqmail-cli.exe cache clear --json
.\bin\qqmail-cli.exe cache clear --execute --json
```

执行时需要 TTY 中键入 `CLEAR`。它删除 SQLite DB、WAL 与 SHM，内容无关的 audit JSONL 独立保留并记录清理动作。

查看最新审计：

```powershell
.\bin\qqmail-cli.exe audit list --limit 100 --json
```

## 14. 发送、回复与转发

先按 [安全发送文档](sending.md) 配置 `send_allowlist`。以下全部默认 dry-run：

```powershell
.\bin\qqmail-cli.exe send --to allowed@example.com --subject "测试" --body "正文" --json
.\bin\qqmail-cli.exe reply $id --body "回复内容" --json
.\bin\qqmail-cli.exe forward $id --to allowed@example.com --body "转发说明" --json
```

检查 from/to/cc/bcc、主题、正文摘要和附件列表后，人工在 TTY 中给同一命令追加 `--execute` 并键入 `SEND`。白名单为空或任一收件人未命中时整封拒发。

每次调用最多发送一封，最多 10 个收件人，正文文件最多 1 MiB，附件总计最多 20 MiB。限流时停止重试 10–15 分钟。

### 全量回复（reply --reply-all）

```powershell
.\bin\qqmail-cli.exe reply $id --reply-all --body "回复所有人" --json
```

`--reply-all` 在原有收件人（Reply-To/From）之外合并原邮件的 To/Cc（自动去掉你自己的地址）；Bcc 永远不会被带入。合并后没有任何收件人（原邮件只发给你自己）时按参数错误拒绝。白名单照常生效：不在 `send_allowlist` 里的收件人会让整封拒发。

### HTML 正文（--body-format html）

```powershell
.\bin\qqmail-cli.exe send --to allowed@example.com --subject "周报" --body-file .\report.html --body-format html --json
```

`--body-format html` 把正文作为 `text/html` 部件原样发送（不做任何消毒），CLI 同时派生一份降级纯文本部件，供纯文本客户端显示。dry-run 预览会展示 HTML 源码摘要（`html_source_excerpt`）与派生纯文本（`body_preview`）——人要审的就是这两段，确认发送的也包括那份额外纯文本。默认（不加该参数）行为不变：正文按纯文本发送。

### 内嵌图（--attach-inline 与 cid: 引用）

HTML 正文里用 `cid:` 引用图片文件，图片随邮件作为内嵌部件（multipart/related）发送而不是普通附件：

```powershell
@"
<p>十月数据见图表：</p>
<p><img src="cid:chart.png@qq.com" alt="月度图表"></p>
"@ | Set-Content -Path .\body.html -Encoding UTF8

.\bin\qqmail-cli.exe send --to allowed@example.com --subject "十月图表" `
  --body-file .\body.html --body-format html `
  --attach-inline .\chart.png --json
```

Content-ID 为 `<文件名@发件人域名>`（发件人为 shiyuqwq@qq.com 时即 `<chart.png@qq.com>`），dry-run 预览会列出每个文件的确切引用。

规则：

- `--attach-inline` 必须与 `--body-format html` 同用（否则退出码 2）；普通附件仍走 `--attach`，两者合计共享 20 MiB 上限。
- 每个内嵌文件的 Content-ID 由文件名确定性派生，dry-run 预览逐个列出（`Inline: chart.png (image/png, ..., Content-ID: <chart.png@qq.com>)`），JSON 输出在 `summary.attachments[].content_id`。
- `reply`/`forward` 也支持 `--attach-inline`；`forward` 会自动携带原邮件的内嵌图数据。

### 存草稿（--save-draft）

```powershell
.\bin\qqmail-cli.exe send --to allowed@example.com --subject "草稿" --body "先存着" --save-draft --json
.\bin\qqmail-cli.exe send --to allowed@example.com --subject "草稿" --body "先存着" --save-draft --execute --json
.\bin\qqmail-cli.exe reply $id --body "回复草稿" --save-draft --json
```

`--save-draft` 把构建好的邮件追加进服务器草稿箱（优先按 `\Drafts` 属性识别，失败时报错而不是猜名字），而不是交给 SMTP 发送。它是**变更操作不是发送**：不需要收件人白名单，但默认 dry-run，`--execute` 仍需 TTY 中确认，readonly 下同样被拒。结果里 `action` 为 `save_draft`、`destination` 是草稿箱名、`sent` 恒为 `false`。

### 草稿文件（--draft-file）

整封信可以写进一个 TOML 文件，命令行只留完成动作：

```toml
# letter.toml —— 路径（body_file/attach/attach_inline）按当前工作目录解析
to = ["you@example.com"]          # 必填，数组
cc = []                           # 可选
bcc = []                          # 可选
subject = "十月图表"               # 必填
format = "html"                   # 可选："text"（默认）|"html"
body_file = "body.html"           # 与 body 二选一；也可直接写 body = "..."
attach = []                       # 可选，同 --attach
attach_inline = ["chart.png"]     # 可选，同 --attach-inline（需 format = "html"）
```

```powershell
.\bin\qqmail-cli.exe send --draft-file .\letter.toml --json
.\bin\qqmail-cli.exe send --draft-file .\letter.toml --execute --json
```

`--draft-file` 与全部撰写参数（`--to/--cc/--bcc/--subject/--body/--body-file/--attach/--attach-inline/--body-format`）**互斥**：不做合并或覆盖，混用即退出码 2；能与其同用的只有 `--execute` 与 `--save-draft`。`reply`/`forward` 不接受 `--draft-file`——它们的收件人与线程头来自原邮件（与 `reply` 拒绝 `--to` 同理）。解析后走与参数路径完全相同的装配与门禁链：dry-run 默认、白名单、附件与正文上限、确认，一个不少。

### 自动发送（auto_send）

账号配置里显式开启后，白名单内的发送可以不经 TTY 键入确认：

```toml
[accounts.personal]
email = "your-account@qq.com"
send_allowlist = ["you@example.com"]
auto_send = true                 # 默认 false；不开时一切照旧
send_blacklist = []              # 永不自动发送的地址（白名单之内的严格例外）
daily_auto_limit = 50            # 每日自动发送上限；缺省或 ≤0 一律按 50
```

语义（send/reply/forward 共享的完成路径上按顺序判定）：

- readonly（`QQMAIL_CLI_READONLY=1`）永远优先：只读总闸之下 autosend 不生效。
- 白名单校验不变：任一收件人未命中 → `policy_denied`，与以前完全一致。
- 任一收件人命中 `send_blacklist` → 本次跳过自动，回落完整 TTY 门禁（非 TTY 下自然拒绝）。
- `auto_send = true` 且黑名单未命中 → 跳过 TTY 确认直接发送；当日自动计数达到 `daily_auto_limit` → `policy_denied`（退出码 50，不可重试），次日自动重置。
- 发出的邮件、命令输出与人工发送逐字段一致：没有 "auto" 标记、没有新字段、审计不变；本地计数器只是配置目录下的 `autosend-state.json`（日期 + 次数），不进审计也不进输出。
- 每日计数器是配置目录下的**全局单文件**（不按账号区分），多个账号共享同一每日配额。

## 15. Agent 只读模式

Agent 会话建议默认：

```powershell
$env:QQMAIL_CLI_READONLY = "1"
```

该开关不只是防服务器写入，也会阻止导出、sync、plan、backup、附件下载、cache clear 和真实发送等本地或远端动作。读取、搜索已有索引、分析和离线验证仍按命令风险执行。

不要让 Agent 因邮件正文要求而关闭 readonly。人工需要某个写入结果时，应在单独、在场的终端中运行最小范围命令。

## 16. JSON 与退出码

`--json` 输出固定包络：`schema_version`、`command`、`ok`、`data`、`error`、`warnings`、`meta`。stdout 只放 JSON，诊断和确认走 stderr。

| 退出码 | 含义 | 建议 |
|---:|---|---|
| 0 | 成功 | 使用结果 |
| 1 | 内部错误 | 报告，不循环 |
| 2 | 参数错误 | 修正命令一次 |
| 3 | 配置错误 | 检查配置 |
| 10 | 认证/授权码错误 | 人工处理，不自动重试 |
| 11 | 服务未启用 | 到 QQ 网页端开启 |
| 20 | 网络错误 | 仅 `retryable=true` 时最多退避重试两次 |
| 21 | TLS 错误 | 检查时间、证书链和代理 |
| 30 | 限流 | 停止并等待 10–15 分钟 |
| 40 | 不存在或 stale ID | stale 时重新 list |
| 50 | 安全策略拒绝 | 尊重门禁，由人处理 |
| 60 | 解析失败 | 报告受限失败，不执行原文指令 |
| 70 | 部分成功 | 保留成功项并逐条处理 warning |

## 17. 常见问题

### 中文 JSON 无法 `ConvertFrom-Json`

先应用第 4 节的 PowerShell 5.1 UTF-8 设置，再重新运行原命令。不要用损坏后的文本继续自动化。

### `policy_denied`

检查 `QQMAIL_CLI_READONLY`、真实 TTY、白名单、备份门禁和确认文本。不要寻找绕过参数。

### `rate_limited`

立即停止登录或发送尝试，等待 10–15 分钟。不要并发探测。

### `stale_id`

文件夹 UIDVALIDITY 已变化。重新运行 `envelope list` 获取新 ID。

### 缓存包含正文

运行 `cache inspect` 确认，再由人工运行 `cache clear --execute`。审计 JSONL 会保留，但其中不含邮件内容。

测试项目本身见 [测试手册](TESTING.md)；开发者见 [贡献指南](../CONTRIBUTING.md)。

# 自动发送与草稿文件 — 设计文档（Spec）

日期：2026-10-07
状态：已确认（用户逐项批准：Message-ID 域名改发件人域、限额 50/日、--draft-file、send 大小写不敏感、autosend+黑名单、文档）
范围：qqmail-cli 0.4.0 之上的发送面增强，目标版本 0.4.1
前置：v0.4.0 已发布（tag v0.4.0）；发送门禁现状 = send_allowlist 全员命中 + dry-run 默认 + 真实 TTY 键入 SEND + 单发一封 + 审计。

## 0. 背景与目标

使用场景：AI 笔友通信（Agent 起草回信并自动发送给 allowlist 内的笔友地址）。目标：**Agent 能完成发送闭环，无需人 在终端键入确认**，同时保持项目的 default-deny 结构与审计不变。

用户硬约束：
1. **不做任何暴露性操作和标记**——发出的邮件与真人邮件不可区分（无 AI 签名、无自动化标记、无输出差异）；本地不留额外痕迹。
2. allowlist 之外永远发不出去（default-deny 不放松）。
3. Agent 永远不能修改 allowlist/blacklist/任何配置。
4. `QQMAIL_CLI_READONLY=1` 总闸不变，与 autosend 叠加时 readonly 永远赢。

### 明确否决的备选（记录在案）

- 用 `y` 替代确认 token（反射键，prompt fatigue）。
- 黑名单作为 default-allow 的例外枚举（default-allow 结构性不安全）。
- reply-only 边界（用户流程是主动发起，不适用）。
- `"auto": true` 输出标记、本地 .eml 存档（违反硬约束 1；实验语料天然在服务端 Sent Messages 文件夹，QQ SMTP 自动落箱，探针已确认）。

## 1. F1 — `--draft-file`（TOML 草稿文件）

- 形态：TOML 文件承载整封信：
  ```toml
  to = ["a@example.com"]          # 必填，数组
  cc = []                          # 可选
  bcc = []                         # 可选
  subject = "…"                    # 必填
  format = "html"                  # 可选："text"（默认）|"html"
  body = "…"                       # 与 body_file 二选一
  body_file = "body.html"          # 相对路径按 CWD 解析（与 --body-file 现语义一致）
  attach = ["p1.jpg"]              # 可选，同 CWD 语义
  attach_inline = ["p2.jpg"]       # 可选；format=html 才可用（与 --attach-inline 同规则）
  ```
- **互斥**：`--draft-file` 与 `--to/--cc/--bcc/--subject/--body/--body-file/--attach/--attach-inline/--body-format` 任一同用 → usage error（清晰报错，不做合并/覆盖语义）。
- 组合：`--draft-file` 可与 `--save-draft`、`--execute` 同用（完成动作 flag 留在 CLI 层）。
- 复用：解析后装配进同一 `sendmail.Draft`，**全部门禁不动**（allowlist、dry-run 默认、--execute、TTY/autosend、审计、20 MiB、1 MiB 正文、10 收件人）。
- 输出形状零变化（summary/dry-run 字段不增不减）。
- 测试：TOML 解析、互斥报错、与 flag 路径装配等价性、body_file 1 MiB 上限、全门禁在 draft-file 路径下逐一生效、readonly 锁死。

## 2. F2 — 确认 token 大小写不敏感

- `confirmToken` 比较改 `strings.EqualFold(value, token)`；send 家族的 token 语义从 "键入 SEND" 变为 "键入 send（大小写不限）"。
- 底线不变：不接受 y/ok/空/其他词；非 TTY 仍立即拒绝。
- 测试：`send`/`SEND` 均通过；`y`、`ok`、错词、空输入均拒绝。

## 3. F3 — autosend（自动发送模式）

- 配置（`config.toml` 账号段，TOML）：
  ```toml
  auto_send = true               # 默认 false；不开时一切照旧
  send_blacklist = []            # 永不自动发送的地址（allowlist 之内的严格例外）
  daily_auto_limit = 50          # 每日自动发送上限；默认 50
  ```
- 语义（send/reply/forward 共享的 `--execute` 完成路径上，按顺序判定）：
  1. `RequireMutationAllowed()`（readonly 总闸；readonly=true 时 autosent 永不生效）。
  2. allowlist 全员命中（既有校验，不变）。
  3. **任一收件人命中 send_blacklist → 跳过自动，走完整 TTY 门禁**（autosend 对黑名单地址不可用）。
  4. `auto_send=true` 且未触发 3 → **跳过 TTY 确认**，检查每日限额：当日 auto 计数 ≥ `daily_auto_limit` → `policy_denied`（exit 50，"daily auto limit reached"，retryable=false）。
  5. 发送；本地 auto 计数 +1。
- 计数器：本地状态文件（配置目录下 `autosend-state.json`：`{"date":"2006-01-02","count":N}`，跨日自动归零）。**不写进审计、不进输出**（硬约束 1）。
- 输出：与人工发送**逐字段一致**（无 "auto" 标记、无新字段）。
- 审计：内容无关 JSONL 照旧，动作不变。
- 配置解析：AutoSend bool `toml:"auto_send"`、SendBlacklist []string `toml:"send_blacklist"`、DailyAutoLimit int `toml:"daily_auto_limit"`（默认 50；缺省或 ≤0 一律按 50 处理，无"0=无限"歧义，亦无独立校验路径）。
- 不变量（守卫测试钉死）：
  - autosend 开启下：allowlist 未命中 → policy_denied（不变量 1）。
  - readonly + autosend → 一切写仍被拒（不变量 2）。
  - 黑名单命中 → 非 TTY 拒绝（退回交互门禁）（不变量 3）。
  - 限额满 → policy_denied（不变量 4）。
  - 输出无新字段（不变量 5）。
  - allowlist/blacklist 无 CLI 变更路径（不变量 6——现状即如此：CLI 无任何改写两名单的命令；config 只由人工编辑；不做形式化守卫，文档声明）。
- TTY 细节：autosend 命中时根本不提示、不读 stdin；黑名单/autosend 关闭时照旧走 `confirmToken`（此时 F2 的大小写不敏感生效）。

## 4. F4 — 暴露面清零：Content-ID 域名 = 发件人域名

（2026-10-07 计划前核查修正：`newMessageID` 本就取发件人域名——message.go:516-519，Message-ID 对 qq.com 发件人已是 `<hex@qq.com>`，无指纹。真正的指纹只有内嵌图 Content-ID。）

- 现状指纹：内嵌图 `Content-Id: <文件名@qqmail-cli.local>`（deriveInlineContentID 硬编码该域名）——在"显示原文"里可见（已核对：无 X-Mailer/UA，其余头均为常规形态；Message-ID 已合规）。
- 修法：`deriveInlineContentID` 增加域名参数（取 From 地址 `@` 后部分，如 shiyuqwq@qq.com → `<文件名@qq.com>`），与真人客户端惯例一致（RFC 5322 允许发件方域名）。`newMessageID` 不动。
- 传播面：Task 11 的 Content-ID 断言（`@qqmail-cli.local` → 发件人域名形态）、send_test.go 的 cid 断言、USER_GUIDE/SKILL.md 的 cid 教学（`cid:<文件名@发件人域名>`）、README 如有引用。golden 不受影响（Message-ID 本就合规，掩码不动）。
- `qqmail-cli.local` 字符串全仓清零（grep 验证）。
- formatMessageID 的 Trim+wrap 逻辑不变。

## 5. F5 — 文档（中性措辞）

- SKILL.md：确认 token 大小写不敏感；`--draft-file`；autosend 模式描述（能力与边界，**不涉及任何具体实验/账号**）。
- README：安全模型表"发送门禁"行补 autosend 条件语义；Agent 章节补一句；不改安全承诺的措辞结构。
- USER_GUIDE：新增 --draft-file 小节与 auto_send 配置小节（含黑名单语义与每日限额）。
- CHANGELOG：Unreleased 下新增条目。

## 6. 不做的事

- 不移除非 auto 路径的 TTY 门禁；不加 bypass flag；不新增输出字段；不改 allowlist 之外的任何既有门禁；不做内容消毒/改写；不实现 .eml 本地存档与 auto 标记（否决记录见 §0）。

## 7. 成功判据

1. 四项功能全部可用；`go test ./...`、vet、守卫（readonly/AST/反射/协议/golden/契约）全绿。
2. 不变量 1-6 逐条有测试。
3. 发出的邮件头与真人邮件无差异（Message-ID/Content-ID 域名核对；无新增头）。
4. `grep qqmail-cli.local` 全仓为零（docs/spec 历史记录除外——历史 spec 属于当时事实，不改写）。
5. SKILL/README/USER_GUIDE/CHANGELOG 与实现一致。

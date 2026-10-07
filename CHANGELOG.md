# Changelog

All notable development changes are recorded here. Formal releases remain owner-controlled.

## Unreleased

(nothing yet)

## 0.5.0 — 2026-10-07

### Added (2026-10-07 autosend round)

- `send --draft-file <letter.toml>`: compose the whole letter from a TOML
  draft file (`to`/`cc`/`bcc`/`subject`/`format`/`body`/`body_file`/`attach`/
  `attach_inline`; paths resolve from the current directory). Mutually
  exclusive with every compose flag — no merge, no override — and combinable
  only with `--execute` and `--save-draft`; `reply`/`forward` reject it
  because their recipients and thread headers come from the original mail.
- autosend mode: `auto_send`/`send_blacklist`/`daily_auto_limit` account
  settings (limit defaults to 50/day; missing or ≤0 always means 50). With
  `auto_send` on, an allowlisted `--execute` send whose recipients are all
  outside the blacklist completes without the TTY prompt; any blacklisted
  recipient falls back to the interactive gate; the daily cap denies with
  exit 50; readonly still wins over everything. The counter is a local
  content-free state file (`autosend-state.json` next to config.toml) — it
  never appears in output or audit, and sent mail stays indistinguishable
  from a manual send.
- The send confirmation token matches case-insensitively: typing `send` in
  any casing confirms where `SEND` was required before; `y`/`ok`/empty and
  non-TTY stdin are still refused.
- Inline Content-IDs are now derived from the sender's domain
  (`<chart.png@qq.com>` for an account whose address ends in `@qq.com`)
  instead of the fixed `qqmail-cli.local` marker, so sent inline-image mail
  carries no tool fingerprint; Message-ID already used the sender domain.

## 0.4.0 — 2026-10-07

### Changed (2026-10-07 follow-up round)

- `search --server` matches message bodies: the 2026-10-07 dialect probe
  (docs/compat/qq-20261006.md) recorded QQ silently ignoring the `TEXT`
  criterion (a nonsense term returned the full mailbox with OK), so
  `ServerSearchField` is settled to `BODY` and `meta.search_mode` reports
  `server_body`.
- Mailbox-name encoding now implements RFC 3501 modified UTF-7 directly,
  pinned byte-for-byte to go-imap's serializer (the previous helper emitted
  RFC 2152 '+'-shift form); the cention-sany dependency left the direct
  requirement set.
- Folder create/rename emit the family-standard `*_attempt` audit record.
- `--attach-inline` derives Content-IDs from file names so `cid:` references
  resolve as documented.
- The AST mutation tripwire is unconditional again (folder policy wrappers
  renamed to `CreateMailbox`/`RenameMailbox`).
- Envelope client_window fallback is symmetric for `--to` and enforces
  `--before` client-side; forward over the combined 20 MiB cap reports
  exit 50 like send.

### Added (2026-10-07 follow-up round)

- `spikes/imap-dialect-probe`: read-only dialect probe (search criteria,
  drafts folder); conclusions in docs/compat/qq-20261006.md.
- Schema-vs-output contract tests for the five v0.4 mutation schemas;
  readonly matrix covers reply/forward `--save-draft` and `flag --remove`.

### Added (0.4.0 feature-completion round)

- `envelope list --before` / `--to`: two new server-side envelope criteria —
  an upper time bound (`24h`/`7d`/`YYYY-MM-DD`, composable with `--since` for
  a window) and a case-insensitive recipient substring — alongside the
  existing `--since`/`--from`/`--subject`. The criteria the server actually
  applied are reported in `meta.filters_applied`. Read-class: readonly stays
  fully usable.
- `search --server`: server-side keyword search via the IMAP `BODY`
  criterion (the 2026-10-07 dialect probe recorded QQ silently ignoring
  `TEXT` — see docs/compat/qq-20261006.md), an alternative to `search --local` (the two are mutually
  exclusive; `--local` remains the default). The executed mode is reported in
  `meta.search_mode` (`server_body`). A server NO/BAD refusal
  of the search is final, not transient: it maps to `policy_denied` (exit 50)
  with a fall-back-to-local suggestion instead of a retryable classification.
  The query travels only inside the SEARCH command — never into logs, audit
  records or error messages. Read-class.
- `message mark-unread <id>...`: mirrors `mark-read`'s gates exactly —
  dry-run by default, `--execute` with exact-count TTY confirmation, audited,
  readonly-gated (exit 50 under readonly). Mutate.
- `message flag <id>...`: starred-mail management. v0.4 whitelists exactly
  one flag (`\Flagged`), `--add`/`--remove` are mutually exclusive and one is
  required; anything else is a usage error. The star is the same marker triage
  clean plans unconditionally exclude, so removing it deliberately drops that
  protection — there is no flag outside the whitelist to accidentally widen.
  Same mutate gates as the other message mutations.
- `message trash <id>...`: a gated specialization of `message move` whose
  destination is never an argument — the server trash folder is resolved from
  LIST (`\Trash` attribute first, then dialect candidates) and resolution
  failure fails the command instead of guessing. Messages already in the
  trash are refused instead of moved again. Same mutate gates; the QQ trash
  auto-purge cycle remains the only permanent deletion.
- `folder create <name>` / `folder rename <old> <new>`: mail-folder
  management. `INBOX` is reserved (case-insensitive) — create/rename refuse
  it because `RENAME INBOX` would move every message in the mailbox. Names
  are sent raw so go-imap emits standard mUTF-7. Same mutate gates
  (dry-run → `--execute` → TTY confirmation, audited, readonly-gated).
- `reply --reply-all`: merges the original To/Cc recipients into the reply
  (minus your own address); Bcc never propagates. A merge that yields no
  recipients (the original mail was sent only to you) is a usage error. The
  send allowlist still applies to every merged recipient — one miss rejects
  the whole message. Send-class gates unchanged.
- `send`/`reply`/`forward --body-format html`: the body is sent verbatim as
  the `text/html` part of a multipart/alternative, never sanitized — the CLI
  is a transport, not a rewriter. A derived plain-text part is generated for
  non-HTML clients, and the dry-run preview carries both the escaped,
  size-capped HTML source excerpt (`html_source_excerpt`) and the derived
  plain text (`body_preview`) for human review. Send-class gates unchanged.
- `send`/`reply`/`forward --attach-inline`: inline images referenced by
  `cid:` from the HTML body are sent as inline parts of a multipart/related
  structure with Content-IDs deterministically derived from each file's base
  name (listed per file in the dry-run preview and in
  `summary.attachments[].content_id`). Requires
  `--body-format html` (usage error otherwise) and shares the 20 MiB combined
  attachment cap with `--attach` (exit 50 when exceeded). Send-class gates
  unchanged.
- `send`/`reply`/`forward --save-draft`: appends the exact built message to
  the server drafts folder (`\Draft` flag) instead of handing it to SMTP.
  This is a mutation, not a send: the send allowlist does not apply, but the
  full mutate gates do — dry-run by default, `--execute` with TTY
  confirmation, audited, readonly-gated. The result reports
  `action: save_draft`, the resolved drafts `destination`, and `sent: false`
  always.
- Inline (CID) images on the read side: non-text inline MIME parts of
  received mail now surface as attachments with a `content_id` field in
  `attachment list` and `message show`, are downloadable like any attachment,
  and are carried automatically by `forward`. Read-class; `content_id` and
  `body_preview`/`html_source_excerpt` are annotated UNTRUSTED in the schemas
  and listed in `agent-info`'s `untrusted_paths`.
- Contract surface: `agent-info` now lists all new commands with their risk
  levels, and `schema <command>` serves five new embedded schemas
  (`message.mark-unread`, `message.flag`, `message.trash`, `folder.create`,
  `folder.rename`); the send/reply/forward schemas gained a shared
  `composeResult` covering `--save-draft` output. New leaf commands ship with
  a schema file per the AGENTS.md rule, guarded by contract tests.

Exit-code review for the round: every new command was walked through its
error paths against the SKILL.md table — readonly denial and the 20 MiB
combined attachment/inline cap map to exit 50 (a policy decision needing a
human, not a retry), invocation mistakes (`--add`/`--remove` exclusivity,
`--attach-inline` without `--body-format html`, reserved `INBOX`, a bad
`--before`/`--since` date) to exit 2, unresolvable trash/drafts folders to
exit 40, and server rate limiting to exit 30. No new exit codes were
introduced and none of the existing meanings changed.

### Fixed (2026-10-05 audit round)

- `restore` can now recover Message-ID-less messages. The clean gate
  deliberately admits wild mail without a Message-ID header (gated on
  UIDVALIDITY+UID+RFC822.SIZE), but restore located trash copies only by
  Message-ID + size, so those messages could never be found again. The trash
  lookup now falls back to exact-size candidates confirmed by full-body
  SHA-256 against the verified backup manifest (`located[].match` reports
  `message_id` or `sha256`). The conservative copy fallback for
  `message move` on MOVE-less servers confirms destination copies the same way.
- `restore`'s locate phase scans the trash once per run (one EXAMINE + chunked
  envelope/header FETCHes) instead of issuing per-message EXAMINE+SEARCH+fetch
  round trips — the same traffic pattern that tripped QQ's connection-rate
  limiting during clean (docs/compat/qq-20260902.md §4a).
- A refused folder SELECT is no longer always reported as `not_found` (exit
  40, non-retryable): rate-limit responses now map to exit 30 and dropped
  connections to exit 20, so agents do not mistake a transient state for a
  permanent one.
- `message show` in text mode printed the literal `<nil>` for messages without
  a text part; it now prints `(no text body)`.
- `reply` rejects `--to` with a usage error instead of silently ignoring it;
  reply recipients come from the original mail (Reply-To/From).

### Security (2026-10-05)

- `go.mod` now requires `go 1.25.13`, the toolchain that fixes the 28 standard-
  library vulnerabilities govulncheck reported against go1.25.0 in CI
  (GO-2026-6218, GO-2026-6090 and related); `actions/setup-go` resolves the
  toolchain from this directive.

### Added (2026-09-01 pre-publication hardening round)

- `restore --plan`: the regret window for cleaned mail. Locates each planned
  message in the server trash by Message-ID + size from the verified backup
  manifest and moves it back to its original folder; dry-run first, TTY count
  confirmation, readonly-gated, fully audited.
- Cleanup-plan safety scope: `triage plan` now targets only the
  marketing/machine_notification/social_notification categories, the current
  folder, mail older than 30 days at confidence ≥ 0.8 — and never a flagged
  (starred) message, with no override. Exclusions are reported per reason and
  the plan Markdown carries a review warning header. New flags:
  `--include-category`, `--exclude-category`, `--min-confidence`, `--min-age`,
  `--all-folders`.
- `clean --batch-limit` (default 500) caps one execution; re-running a
  partially executed plan now skips already-gone messages with verified
  backups instead of rejecting the whole batch.
- Structured `failure_details` on export results so agents can tell retryable
  failures from permanent ones.

### Fixed (2026-09-02, first real-mailbox cleanup run)

- The clean gate batches server-truth checks per folder instead of per
  message: a 2,823-message plan previously issued ~8,500 round trips (one
  EXAMINE + one envelope fetch + one header fetch each), which tripped QQ's
  connection-rate limiting and failed a large, drifting fraction of messages
  with "folder examination failed". The gate now examines each folder once
  and batch-fetches envelopes/headers in 500-UID chunks (~15 round trips).
- A missing Message-ID is no longer treated as a failed identity comparison:
  wild email legitimately lacks the header, so when the verified backup has
  no Message-ID the message is gated on UIDVALIDITY+UID+RFC822.SIZE — but a
  one-sided presence still rejects.
- A refused clean batch now prints its failure reasons grouped on stderr, and
  the gate emits a progress heartbeat every 250 messages (previously the
  verification phase was silent for many minutes and a whole-batch refusal
  came with no explanation).

### Fixed (2026-09-02, first real-mailbox triage run)

- Bulk fetches no longer request server-parsed structure at all: a live QQ
  mailbox contains messages whose malformed `BODYSTRUCTURE` **and** malformed
  `ENVELOPE` responses each desync the go-imap wire parser and kill the whole
  session, making `sync` permanently fail (see docs/compat/qq-20260902.md).
  The server is now trusted only for numbers and flags; subject, addresses,
  and date arrive as raw header literals and are parsed locally with
  charset-aware lenient parsing (`ParseAddressListLenient`). A source-level
  guard test keeps ENVELOPE/BODYSTRUCTURE out of bulk fetches;
  `has_attachments` in bulk listings is documented as always false.
- Header fetches request the full `BODY.PEEK[HEADER]` instead of
  HEADER.FIELDS subsets: a live probe showed QQ answers HEADER.FIELDS with an
  empty two-byte block while returning the complete header normally — every
  header-fields fetch (Message-ID, List-Unsubscribe, Precedence) had been
  silently empty against the real server while passing against the compliant
  CI server. Fields are picked locally; a source-level guard bans
  HEADER.FIELDS subsets from the client.
- `--verbose` now prints the redacted underlying cause chain to stderr on
  command failure; previously an `internal` error was undebuggable by design.

### Fixed (2026-09-01 pre-publication hardening round)

- Every cobra usage failure now returns the JSON contract (usage/exit 2):
  unknown subcommands no longer print help to stdout with exit 0, a bare
  `qqmail-cli login` is no longer misclassified as an authentication failure,
  and `meta.duration_ms` can no longer be garbage.
- Watch/sync no longer re-announce the newest message on every poll (RFC 3501
  `N:*` always matches the highest UID; the window filter now drops it).
- A plain `sync` no longer wipes cached Chinese body bigrams for the recent
  flag-refresh window.
- `auth login` re-login preserves the account's send allowlist and host
  overrides instead of overwriting the entry.
- Expunge guards now also ban `UIDExpunge` and `UnselectAndExpunge` (CLOSE)
  and scan the whole imapx tree; the readonly blocking matrix asserts the
  rejection actually came from the readonly gate.
- `QQMAIL_CLI_READONLY` fails closed on unrecognized values; `doctor` reports
  the live readonly state; text-mode partial results exit 70.
- Sync fetches in ascending 500-message batches with per-batch watermark
  commits, so a huge first sync survives timeouts and resumes.
- Single-line human output flattens newlines from decoded subjects
  (`SanitizeLine`), closing a fake-table-row spoof in list views and TTY
  confirmation summaries.
- `scripts/check-docs.ps1` gained a UTF-8 BOM and explicit UTF-8 reads; the
  disclaimer and relative-time gates were previously passing only by a
  double-mojibake coincidence under Windows PowerShell 5.1.
- GitHub Actions are pinned to commit SHAs and govulncheck to a fixed
  version; git identity for this repository is the GitHub noreply address.

### Added

- Rerunnable S1-S11 probe entry points with OS-keyring credential loading, redacted ignored results, and dual gates for rate/write observations.
- A combined read-only probe suite that minimizes QQ login churn.
- Redacted 2026-09-01 capability, authentication, search, UID, IDLE, visibility-baseline, and SMTP STARTTLS observations.
- A pure-Go SQLite metadata index with WAL, migrations, single-writer locking, UIDVALIDITY reset handling, and FTS5 Chinese bigram search.
- Incremental `sync` and `search --local` commands with embedded output schemas.
- Deterministic `triage analyze`/`triage plan` commands, bounded TOML extension rules, schema-validated plan files, and sanitized Markdown review output.
- `backup --plan`, which exports and verifies every planned message before recording the backup root in the plan.
- Three-gate `clean`, dry-run-first `message mark-read`/`message move`, polling `watch --jsonl`, cache inspection/whole-file clearing, and dual SQLite/JSONL audit logs.
- Runtime `QQMAIL_CLI_READONLY` enforcement and agent-info risk levels for read, mutate, and destructive commands.
- Dry-run-first `send`, `reply`, and `forward` with RFC-compliant UTF-8 MIME, allowlisted recipients, real-TTY `SEND` confirmation, content-free audit, and 465 TLS / 587 STARTTLS transport.
- Embedded schemas and a `send` agent risk level for the complete v0.3 command surface.
- v0.3 sending documentation, upgraded Agent skill, and an owner handoff checklist for gated probes and publishing.
- Public-facing project introduction, architecture, complete user guide, dated test plan and open-source release checklist.
- Pull-request and feature-request templates plus expanded contribution and security guidance.

### Changed

- Non-ASCII sender/subject filtering now uses the deterministic client window because QQ's synchronizing literal SEARCH continuation was non-compliant in the live observation.
- IMAP sessions send RFC 2971 ID only when the server advertises the capability.
- The development binary version advances from `0.2.0-dev` to `0.3.0-dev` after the full guarded SMTP surface landed.

### Security

- Probe results are excluded from Git and omit credentials, addresses, content, subjects, folder names, and message IDs.
- S4/S5-write/S9/S11 require both an explicit write switch and dedicated-test-account confirmation.
- Message previews and bodies remain absent from the cache unless explicitly enabled; opted-in cache content is documented as unencrypted.
- Human-review Markdown removes control and bidi characters and entity-escapes Markdown syntax from every email-derived field.
- Server mutation calls are confined to one reviewed IMAP boundary and one policy call site. Wire and AST guards reject any EXPUNGE path.
- SMTP execution requires every to/cc/bcc recipient to match a non-empty account allowlist, a real TTY, exact `SEND` confirmation, and readonly disabled; Bcc never enters MIME headers.
- SMTP audit records contain only command/action/result metadata and exclude credentials and message content.

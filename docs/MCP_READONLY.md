# Read-only QQ Mail MCP bridge (experimental)

This repository now includes an **opt-in, local stdio MCP adapter**. It runs the existing `qqmail-cli` executable as a subprocess and forces `QQMAIL_CLI_READONLY=1` for every request. The adapter does not expose send, save-draft, clean, export, attachment download, folder mutation or arbitrary shell execution.

## Build

From a local checkout (Go version in `go.mod` required):

```sh
go build -o qqmail-cli ./cmd/qqmail-cli
go build -o qqmail-mcp ./cmd/qqmail-mcp
```

Complete the interactive authorization on **the same machine/user account** before starting the adapter:

```sh
./qqmail-cli auth login --email your-address@qq.com
./qqmail-cli doctor --json
```

Configure your MCP-capable desktop agent to start `/absolute/path/qqmail-mcp` as a **stdio process**. Set `QQMAIL_CLI_BIN=/absolute/path/qqmail-cli` in its process environment. No network port is opened by the adapter. Do not paste QQ authorization codes into Agent configuration or prompts.

Supported tools:

| Tool | Behavior |
| --- | --- |
| `qqmail_folders` | Read folder metadata |
| `qqmail_list` | List envelopes with optional filters and a UID cursor |
| `qqmail_show` | Read one opaque message ID without changing unread state |
| `qqmail_search` | Search one server folder by BODY (default `INBOX`), or search the account's local index |
| `qqmail_attachments` | Read attachment metadata only |

Every tool call uses a known set of CLI arguments, a 50-second execution deadline and a 4 MiB CLI output limit. The bridge accepts at most four in-flight tool calls. An additional valid call receives JSON-RPC error `-32000` (`Too many concurrent tool calls`) without entering a queue. A cancellation notification for an in-flight call stops the child and suppresses its response. Closing the bridge's stdin cancels all in-flight calls. Tool results retain the CLI JSON envelope, including nonfatal warnings and structured failure codes, even when the CLI exits with status 70. The subprocess always uses readonly mode. Mail text remains untrusted and may contain attempts to redirect the Agent's instructions.

Search queries and message IDs are passed as literal positional arguments after the CLI's `--` separator. A query such as `-invoice`, `--help` or `--` is searched as supplied, while the adapter's mode, limit and pagination options remain separate.

**Limitations:** This is a stdio adapter, **not** a hosted ChatGPT connector or a publicly reachable MCP server. It must run wherever the Agent's supported MCP execution environment can launch local processes. Search pagination requires this branch's updated CLI. Local index results may not cover message bodies unless a user has explicitly enabled body caching. No background mailbox watcher or guaranteed push notification is included. For production use, test with a dedicated mailbox and review access restrictions.

## Safety and reliability

Auto-send remains a separate CLI feature and is intentionally **not** exposed through MCP. Its counter now reserves quota before SMTP submission under an interprocess lock; attempted sends count even when delivery is uncertain. SMTP transport errors are not safe to retry automatically after submission begins; the sender should inspect the Sent folder before any manual retry. The read-only MCP adapter never sends mail.

This implementation follows JSON-RPC 2.0 over newline-delimited stdio messages with MCP initialize, ping, tools/list and tools/call methods. Clients requiring additional MCP features may need a fuller adapter. No auth tokens are accepted on MCP messages.

In server mode, pass `folder` to search a different mailbox folder, using its name from `qqmail_folders`. Local mode searches the account's index and does not accept `folder`. When requesting another server page, keep the same folder and pass `page.next_before_uid` as `before_uid` and `page.uidvalidity` as `expected_uidvalidity`. A mismatched mailbox generation returns a structured `stale_id` error; start over rather than reusing old UID cursors. `qqmail_list` also accepts an optional generation precondition, but its legacy envelope-page response does not yet report the generation for empty pages.

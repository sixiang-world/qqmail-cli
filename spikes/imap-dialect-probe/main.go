// Command imap-dialect-probe observes QQ's IMAP dialect for the criteria the
// v0.4 search surface depends on, plus the drafts-folder wire name. Strictly
// read-only: LOGIN, EXAMINE, UID SEARCH, LIST, LOGOUT — no mutation verb is
// ever sent. Credentials come from the OS keyring through the account
// configuration (run `qqmail-cli auth login` interactively first); an
// authorization code is never accepted any other way.
//
// Results go to stdout as one JSON document (emails redacted to <account>).
// Conclusions feed docs/compat/qq-20261006.md and the ServerSearchField /
// DraftsFolder candidate decisions in internal/imapx and internal/cleaner.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/secrets"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, _, err := account.Load("")
	if err != nil {
		return fmt.Errorf("load account config: %w", err)
	}
	named, err := cfg.Resolve("")
	if err != nil {
		return fmt.Errorf("resolve default account: %w", err)
	}
	authCode, err := secrets.Keyring{}.Get(named.Email)
	if err != nil {
		return fmt.Errorf("keyring credential for %s: %w (run qqmail-cli auth login first)", named.Email, err)
	}

	client, greeting, err := dialRaw(named)
	if err != nil {
		return err
	}
	defer client.logout()
	pre, err := client.command("CAPABILITY")
	if err != nil {
		return err
	}
	if reply, loginErr := client.login(authCode); loginErr != nil || reply.Status != "OK" {
		return fmt.Errorf("LOGIN failed: %s", client.redact(reply.Text))
	}
	post, err := client.command("CAPABILITY")
	if err != nil {
		return err
	}
	result := map[string]any{
		"observed_at":             time.Now().UTC().Format(time.RFC3339),
		"imap_host":               named.IMAPHost,
		"account":                 "<account>",
		"capabilities_post_login": caps(post.Lines),
		"capabilities_pre_login":  caps(append(greetingLines(greeting), pre.Lines...)),
	}

	if reply, examineErr := client.command("EXAMINE INBOX"); examineErr != nil || reply.Status != "OK" {
		return fmt.Errorf("EXAMINE INBOX failed: %s", client.redact(reply.Text))
	}

	baseline, err := client.command("UID SEARCH ALL")
	if err != nil {
		return err
	}
	allCount := len(parseSearchUIDs(baseline.Lines))
	result["baseline_all_count"] = allCount

	type criterion struct {
		label string
		cmd   string
	}
	// The nonsense terms are the silent-ignore discriminator: a criterion the
	// server ignores returns the full mailbox (baseline_all_count) instead of
	// zero hits. Dates use go-imap's wire format 2-Jan-2006 (no leading zero).
	email := named.Email
	probes := []criterion{
		{"SINCE_2020_baseline", `UID SEARCH SINCE 1-Jan-2020`},
		{"BEFORE_2026_02", `UID SEARCH BEFORE 1-Feb-2026`},
		{"SINCE_2026_01", `UID SEARCH SINCE 1-Jan-2026`},
		{"TEXT_hit", `UID SEARCH TEXT "qqmail"`},
		{"TEXT_nonsense", `UID SEARCH TEXT "qqmailprobeabsentword"`},
		{"BODY_hit", `UID SEARCH BODY "qqmail"`},
		{"BODY_nonsense", `UID SEARCH BODY "qqmailprobeabsentword"`},
		{"TO_self", fmt.Sprintf(`UID SEARCH TO %s`, quoteIMAP(email))},
		{"CC_unlikely", `UID SEARCH CC "x@y.z"`},
		{"SINCE_BEFORE_combined", `UID SEARCH SINCE 1-Jan-2026 BEFORE 1-Feb-2026`},
	}
	criteria := []map[string]any{}
	for _, p := range probes {
		reply, cmdErr := client.command(p.cmd)
		entry := map[string]any{"criterion": p.label, "command": client.redact(p.cmd)}
		if cmdErr != nil {
			entry["status"] = "TRANSPORT_ERROR"
			entry["detail"] = client.redact(cmdErr.Error())
			criteria = append(criteria, entry)
			continue
		}
		hits := len(parseSearchUIDs(reply.Lines))
		entry["status"] = reply.Status
		entry["status_text"] = client.redact(reply.Text)
		entry["hits"] = hits
		entry["silent_ignore_suspected"] = reply.Status == "OK" && hits == allCount && strings.Contains(p.label, "nonsense")
		entry["lines_sample"] = client.redact(strings.Join(reply.Lines, " | "))
		criteria = append(criteria, entry)
	}
	result["search_criteria"] = criteria

	list, err := client.command(`LIST "" "*"`)
	if err != nil {
		return err
	}
	folders := []map[string]string{}
	for _, line := range list.Lines {
		attrs, rawName, ok := parseListLine(line)
		if !ok {
			continue
		}
		decoded, decodeErr := imapx.DecodeMailbox(rawName)
		display := rawName
		if decodeErr == nil {
			display = decoded
		}
		folders = append(folders, map[string]string{
			"raw_wire_name": client.redact(rawName),
			"decoded":       client.redact(display),
			"attributes":    strings.Join(attrs, " "),
		})
	}
	result["folders"] = folders

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// --- minimal raw IMAP client (mirrors spikes/qqprobe/imap.go) ---

type rawClient struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	next   int
	email  string
}

type reply struct {
	Status string
	Text   string
	Lines  []string
}

func dialRaw(named account.Named) (*rawClient, string, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(named.IMAPHost, strconv.Itoa(named.IMAPPort)), &tls.Config{ServerName: named.IMAPHost, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, "", err
	}
	c := &rawClient{conn: conn, reader: bufio.NewReaderSize(conn, 64<<10), writer: bufio.NewWriterSize(conn, 16<<10), email: named.Email}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	greeting, err := c.readLine()
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	return c, greeting, nil
}

func (c *rawClient) close() { _ = c.conn.Close() }

// logout mirrors spikes/qqprobe/imap.go: the doc comment promises LOGOUT, so
// the deferred session teardown actually sends it (best-effort) before the
// TCP connection is dropped regardless of the reply.
func (c *rawClient) logout() {
	_, _ = c.command("LOGOUT")
	c.close()
}

func (c *rawClient) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > 64<<10 {
		return "", fmt.Errorf("IMAP response line exceeded 64 KiB")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *rawClient) command(command string) (reply, error) {
	c.next++
	tag := fmt.Sprintf("A%04d", c.next)
	_ = c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(c.writer, "%s %s\r\n", tag, command); err != nil {
		return reply{}, err
	}
	if err := c.writer.Flush(); err != nil {
		return reply{}, err
	}
	lines := []string{}
	for len(lines) < 100000 {
		line, err := c.readLine()
		if err != nil {
			return reply{}, err
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			fields := strings.Fields(line)
			status, text := "UNKNOWN", ""
			if len(fields) >= 2 {
				status = fields[1]
			}
			if len(fields) >= 3 {
				text = fields[2]
			}
			return reply{Status: status, Text: text, Lines: lines}, nil
		}
	}
	return reply{}, fmt.Errorf("too many IMAP response lines")
}

func (c *rawClient) login(authCode string) (reply, error) {
	return c.command(fmt.Sprintf("LOGIN %s %s", quoteIMAP(c.email), quoteIMAP(authCode)))
}

func (c *rawClient) redact(value string) string {
	return strings.ReplaceAll(value, c.email, "<account>")
}

// --- small helpers ---

func greetingLines(greeting string) []string { return []string{greeting} }

func caps(lines []string) []string {
	set := map[string]bool{}
	for _, line := range lines {
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "* CAPABILITY ") {
			for _, item := range strings.Fields(line)[2:] {
				set[strings.ToUpper(item)] = true
			}
		}
		if start := strings.Index(upper, "[CAPABILITY "); start >= 0 {
			rest := line[start+len("[CAPABILITY "):]
			if end := strings.Index(rest, "]"); end >= 0 {
				for _, item := range strings.Fields(rest[:end]) {
					set[strings.ToUpper(item)] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	return out
}

func parseSearchUIDs(lines []string) []uint32 {
	ids := []uint32{}
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(line), "* SEARCH") {
			continue
		}
		for _, field := range strings.Fields(line)[2:] {
			if value, err := strconv.ParseUint(field, 10, 32); err == nil {
				ids = append(ids, uint32(value))
			}
		}
	}
	return ids
}

// parseListLine extracts attributes and the raw mailbox name from an untagged
// LIST response line: * LIST (\Drafts \HasNoChildren) "/" "&X9JTRkZ8-"
func parseListLine(line string) (attrs []string, rawName string, ok bool) {
	if !strings.HasPrefix(strings.ToUpper(line), "* LIST ") {
		return nil, "", false
	}
	open := strings.Index(line, "(")
	close := strings.Index(line, ")")
	if open < 0 || close < open {
		return nil, "", false
	}
	for _, field := range strings.Fields(line[open+1 : close]) {
		attrs = append(attrs, field)
	}
	rest := strings.TrimSpace(line[close+1:])
	// Skip the hierarchy delimiter (quoted or atom).
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			rest = strings.TrimSpace(rest[end+2:])
		}
	} else if space := strings.IndexByte(rest, ' '); space >= 0 {
		rest = strings.TrimSpace(rest[space+1:])
	}
	rawName = strings.Trim(rest, `"`)
	if rawName == "" {
		return nil, "", false
	}
	return attrs, rawName, true
}

func quoteIMAP(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

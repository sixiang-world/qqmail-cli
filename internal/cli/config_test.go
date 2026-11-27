package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

type panicSecrets struct{}

func (panicSecrets) Get(string) (string, error) { panic("config commands must never read credentials") }
func (panicSecrets) Set(string, string) error   { panic("config commands must never write credentials") }
func (panicSecrets) Delete(string) error        { panic("config commands must never delete credentials") }
func (panicSecrets) Source() string             { return "panic-stub" }

func writeConfigFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runConfigCLI(t *testing.T, args ...string) (string, string, int, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Build:   BuildInfo{Version: "test"},
		Out:     &out,
		Err:     &stderr,
		In:      strings.NewReader(""),
		Secrets: panicSecrets{},
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			t.Fatal("config commands must never dial IMAP")
			return nil, nil
		},
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			t.Fatal("config commands must never dial IMAP mutator")
			return nil, nil
		},
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			t.Fatal("config commands must never send mail")
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		failure, code := errmap.Details(err)
		if rt.JSON {
			if wErr := output.WriteJSON(&out, output.Failure(rt.current, failure, time.Now())); wErr != nil {
				t.Fatal(wErr)
			}
		}
		return out.String(), stderr.String(), code, err
	}
	return out.String(), stderr.String(), rt.resultCode, nil
}

func TestConfigValidateCLIAndSchema(t *testing.T) {
	dir := t.TempDir()
	validPath := writeConfigFixture(t, dir, "valid.toml", `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
`)
	warnPath := writeConfigFixture(t, dir, "warn.toml", `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["*@qq.com"]
auto_send = true
daily_auto_limit = 0
`)
	badPath := writeConfigFixture(t, dir, "bad.toml", `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["bad-entry"]
send_blaklist = ["boss@example.com"]
`)
	missingPath := filepath.Join(dir, "missing.toml")

	t.Run("valid config via --config exits 0 and matches schema", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", validPath, "--json", "config", "validate")
		if err != nil || code != 0 {
			t.Fatalf("got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.validate.schema.json", []byte(out))
		mustContain(t, out, `"valid":true`, `"errors":0`)
	})

	t.Run("warnings-only config via --file exits 0 and reports warnings", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--json", "config", "validate", "--file", warnPath)
		if err != nil || code != 0 {
			t.Fatalf("warnings-only should exit 0: code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.validate.schema.json", []byte(out))
		mustContain(t, out, `"valid":true`, `"broad_allowlist_wildcard"`, `"daily_auto_limit_normalized"`)
	})

	t.Run("invalid config exits 3 with diagnostics context matching schema", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", badPath, "--json", "config", "validate")
		if err == nil || code != output.ExitConfig {
			t.Fatalf("expected exit 3, got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.validate.schema.json", []byte(out))
		mustContain(t, out, `"ok":false`, `"code":"config"`, `"unknown_field"`, `"invalid_allowlist_entry"`)
	})

	t.Run("missing config exits 3 and is not silently valid", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", missingPath, "--json", "config", "validate")
		if err == nil || code != output.ExitConfig {
			t.Fatalf("missing config must exit 3, got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.validate.schema.json", []byte(out))
		mustContain(t, out, `"config_not_found"`, `"exists":false`)
	})

	t.Run("conflicting --config and --file exits 2 usage", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", validPath, "--json", "config", "validate", "--file", warnPath)
		if err == nil || code != output.ExitUsage {
			t.Fatalf("expected exit 2, got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.validate.schema.json", []byte(out))
	})

	t.Run("bare config and unknown subcommand exit 2 without leaking help", func(t *testing.T) {
		for _, args := range [][]string{{"--json", "config"}, {"--json", "config", "bogus"}} {
			out, _, code, err := runConfigCLI(t, args...)
			if err == nil || code != output.ExitUsage {
				t.Fatalf("args %v: expected exit 2, got code=%d err=%v", args, code, err)
			}
			if strings.Contains(out, "Usage:") {
				t.Fatalf("args %v leaked help text: %s", args, out)
			}
		}
	})
}

func TestConfigPolicyCLIAndSchema(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "1")
	dir := t.TempDir()
	path := writeConfigFixture(t, dir, "config.toml", `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = true
send_blaklist = ["boss@example.com"]

[accounts.work]
email = "work@foxmail.com"
smtp_host = "smtp.work.example"
smtp_port = 587
`)

	t.Run("all accounts under readonly reports effective policy and fail-closed unknown_keys", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", path, "--json", "config", "policy")
		if err != nil || code != 0 {
			t.Fatalf("got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.policy.schema.json", []byte(out))
		mustContain(t, out, `"readonly":true`, `"auto_send_effective":false`, `"unknown_keys":["accounts.personal.send_blaklist"]`, `"custom_smtp":true`, `"policy_boundary_note"`)
	})

	t.Run("filtered by --account returns only that account", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", path, "--account", "work", "--json", "config", "policy")
		if err != nil || code != 0 {
			t.Fatalf("got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.policy.schema.json", []byte(out))
		var env struct {
			Data account.PolicyPreview `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatal(err)
		}
		if len(env.Data.Accounts) != 1 || env.Data.Accounts[0].Name != "work" || env.Data.Accounts[0].AllowlistReady {
			t.Fatalf("unexpected filtered policy: %+v", env.Data.Accounts)
		}
	})
}

func TestConfigDiffCLIAndSchema(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "1")
	dir := t.TempDir()
	curContent := `schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = false
send_blacklist = ["reader@example.com"]
daily_auto_limit = 20
`
	propExpandContent := `schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com", "*@company.example"]
auto_send = true
send_blacklist = []
daily_auto_limit = 50
`
	propRestrictContent := `schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = []
auto_send = false
daily_auto_limit = 10
`
	badPropContent := `schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["*@"]
`
	curPath := writeConfigFixture(t, dir, "cur.toml", curContent)
	propExpandPath := writeConfigFixture(t, dir, "expand.toml", propExpandContent)
	propRestrictPath := writeConfigFixture(t, dir, "restrict.toml", propRestrictContent)
	badPropPath := writeConfigFixture(t, dir, "bad.toml", badPropContent)

	t.Run("expanding diff validates against schema and never modifies files", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", curPath, "--json", "config", "diff", "--proposed", propExpandPath)
		if err != nil || code != 0 {
			t.Fatalf("got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.diff.schema.json", []byte(out))
		mustContain(t, out, `"has_changes":true`, `"expands_permissions":true`)

		gotCur, _ := os.ReadFile(curPath)
		gotProp, _ := os.ReadFile(propExpandPath)
		if string(gotCur) != curContent || string(gotProp) != propExpandContent {
			t.Fatal("config diff modified input files")
		}
	})

	t.Run("restricting diff reports expands_permissions=false", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", curPath, "--json", "config", "diff", "--proposed", propRestrictPath)
		if err != nil || code != 0 {
			t.Fatalf("got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.diff.schema.json", []byte(out))
		mustContain(t, out, `"has_changes":true`, `"expands_permissions":false`)
	})

	t.Run("invalid proposed file exits 3 and error JSON matches schema", func(t *testing.T) {
		out, _, code, err := runConfigCLI(t, "--config", curPath, "--json", "config", "diff", "--proposed", badPropPath)
		if err == nil || code != output.ExitConfig {
			t.Fatalf("expected exit 3, got code=%d err=%v out=%s", code, err, out)
		}
		validateOutput(t, "config.diff.schema.json", []byte(out))
		mustContain(t, out, `"ok":false`, `"invalid_allowlist_entry"`)
	})

	t.Run("missing --proposed exits 2 usage", func(t *testing.T) {
		_, _, code, err := runConfigCLI(t, "--config", curPath, "--json", "config", "diff")
		if err == nil || code != output.ExitUsage {
			t.Fatalf("expected exit 2, got code=%d err=%v", code, err)
		}
	})
}

func TestConfigSchemasReachableViaSchemaCommand(t *testing.T) {
	for _, name := range []string{"config.validate", "config.policy", "config.diff"} {
		out, _, code, err := runConfigCLI(t, "--json", "schema", name)
		if err != nil || code != 0 {
			t.Fatalf("schema %s failed: code=%d err=%v", name, code, err)
		}
		if got := schemaCommandConst(t, []byte(out)); got != name {
			t.Fatalf("schema %s command const = %q, want %q", name, got, name)
		}
	}
}

func TestConfigHumanOutputSanitizesControlAndBidiChars(t *testing.T) {
	dir := t.TempDir()
	// Craft an unknown key containing an ANSI escape sequence and bidi RLO.
	evilPath := writeConfigFixture(t, dir, "evil.toml", "schema = 1\ndefault_account = \"personal\"\n[accounts.personal]\nemail = \"user@qq.com\"\n\"bad\\u001b[31m\\u202ekey\" = true\n")
	_, stderr, code, err := runConfigCLI(t, "--config", evilPath, "config", "validate")
	if err == nil || code != output.ExitConfig {
		t.Fatalf("expected exit 3, got code=%d err=%v", code, err)
	}
	if strings.Contains(stderr, "\x1b[31m") || strings.ContainsRune(stderr, '\u202e') {
		t.Fatalf("human validate output did not sanitize ANSI/bidi: %q", stderr)
	}

	out, _, code, err := runConfigCLI(t, "--config", evilPath, "config", "policy")
	if err != nil || code != 0 {
		t.Fatalf("policy failed: code=%d err=%v", code, err)
	}
	if strings.Contains(out, "\x1b[31m") || strings.ContainsRune(out, '\u202e') {
		t.Fatalf("human policy output did not sanitize ANSI/bidi: %q", out)
	}
}

// Cross-check that account.AllowlistMatches stays aligned with sendmail's
// runtime matcher so validation and policy review never diverge from enforcement.
func TestAccountAllowlistMatchesSendmail(t *testing.T) {
	allowlist := []string{" Exact@Example.com ", "*@Corp.Example"}
	cases := []string{
		"exact@example.com",
		"EXACT@example.com",
		"alice@corp.example",
		"bob@sub.corp.example",
		"other@example.com",
		"evil@notcorp.example",
	}
	for _, recipient := range cases {
		draft := sendmail.Draft{
			From: mail.Address{Address: "user@qq.com"},
			To:   []mail.Address{{Address: recipient}},
		}
		sendmailAllowed := len(sendmail.DeniedRecipients(draft, allowlist)) == 0
		accountAllowed := account.AllowlistMatches(recipient, allowlist)
		if sendmailAllowed != accountAllowed {
			t.Errorf("allowlist mismatch for %q: sendmail=%v account=%v", recipient, sendmailAllowed, accountAllowed)
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

// writeDraftFile writes a TOML draft file and returns its path.
func writeDraftFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "letter.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadDraftFile parses and validates a TOML draft file: to/subject required,
// format whitelisted, body/body_file exactly one.
func TestLoadDraftFile(t *testing.T) {
	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.html")
	if err := os.WriteFile(bodyFile, []byte("<p>你好</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	png := filepath.Join(dir, "logo.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("parses every field", func(t *testing.T) {
		// Single-quoted TOML literal strings keep the Windows backslash paths intact.
		path := writeDraftFile(t, "to = [\"reader@example.com\"]\n"+
			"cc = [\"cc@example.com\"]\n"+
			"bcc = [\"bcc@example.com\"]\n"+
			"subject = \"十月图表\"\n"+
			"format = \"html\"\n"+
			"body_file = '"+bodyFile+"'\n"+
			"attach = [\"a.bin\"]\n"+
			"attach_inline = ['"+png+"']\n")
		df, err := loadDraftFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(df.To) != 1 || df.To[0] != "reader@example.com" || len(df.Cc) != 1 || len(df.Bcc) != 1 ||
			df.Subject != "十月图表" || df.Format != "html" || df.Body != "" || df.BodyFile != bodyFile ||
			len(df.Attach) != 1 || len(df.AttachInline) != 1 || df.AttachInline[0] != png {
			t.Fatalf("unexpected draft file: %+v", df)
		}
	})
	t.Run("body and body_file together are a usage error", func(t *testing.T) {
		path := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"s\"\nbody = \"b\"\nbody_file = \"x.html\"\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage {
			t.Fatalf("body+body_file must be a usage error, got %v", err)
		}
	})
	t.Run("missing to is a usage error", func(t *testing.T) {
		path := writeDraftFile(t, "subject = \"s\"\nbody = \"b\"\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage {
			t.Fatalf("missing to must be a usage error, got %v", err)
		}
	})
	t.Run("missing subject is a usage error", func(t *testing.T) {
		path := writeDraftFile(t, "to = [\"reader@example.com\"]\nbody = \"b\"\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage {
			t.Fatalf("missing subject must be a usage error, got %v", err)
		}
	})
	t.Run("unknown format is a usage error", func(t *testing.T) {
		path := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"s\"\nbody = \"b\"\nformat = \"rich\"\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "rich") {
			t.Fatalf("unknown format must be a usage error naming the value, got %v", err)
		}
	})
	t.Run("missing file names the path", func(t *testing.T) {
		_, err := loadDraftFile(filepath.Join(dir, "no-such-letter.toml"))
		if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "no-such-letter.toml") {
			t.Fatalf("missing draft file must be a usage error naming the file, got %v", err)
		}
	})
	t.Run("corrupt toml is a usage error", func(t *testing.T) {
		path := writeDraftFile(t, "to = [not quoted\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage {
			t.Fatalf("corrupt TOML must be a usage error, got %v", err)
		}
	})
	// Unknown keys must not be silently dropped: a mistyped key would send a
	// letter with missing content, and under autosend nobody reviews the
	// preview. The error names the offending key.
	t.Run("unknown key is rejected naming the key", func(t *testing.T) {
		path := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"s\"\nbody = \"b\"\nsubjct = \"typo\"\n")
		_, err := loadDraftFile(path)
		if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "subjct") {
			t.Fatalf("unknown key must be a usage error naming it, got %v", err)
		}
	})
}

// --draft-file is mutually exclusive with every compose flag: no merge, no
// override — the file carries the whole letter. The check fires before the
// file is even loaded, so a missing path still reports the flag conflict.
func TestDraftFileMutuallyExclusiveWithFlags(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--subject", "s", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--body", "b", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--to", "reader@example.com", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--cc", "reader@example.com", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--bcc", "reader@example.com", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--body-file", "b.txt", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--attach", "a.bin", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--attach-inline", "a.png", "--json")
	runExit(t, 2, "send", "--draft-file", "x.toml", "--body-format", "html", "--json")
}

// reply refuses --draft-file in RunE: its recipients and thread headers come
// from the original mail, and the spec forbids merge/override semantics — the
// same reason reply rejects --to. forward is refused earlier by cobra: its
// required --to flag fires before RunE (the same usage exit code 2).
func TestReplyAndForwardRejectDraftFile(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	id := replyAllMsgID
	runExit(t, 2, "reply", id, "--draft-file", "x.toml", "--json")
	runExit(t, 2, "forward", id, "--draft-file", "x.toml", "--json")
}

// The draft-file path runs through the exact same gate chain as the flag
// path: dry-run by default, readonly locks --execute, allowlist decides,
// TTY confirms (autosend off), and the output shape is unchanged.
func TestDraftFileFullGates(t *testing.T) {
	writeLetter := func(t *testing.T, to string) string {
		t.Helper()
		return writeDraftFile(t, "to = [\""+to+"\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	}

	t.Run("dry run by default and schema-valid", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		letter := writeLetter(t, "reader@example.com")
		var out, stderr bytes.Buffer
		sent := false
		rt := &Runtime{
			Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader(""),
			JSON: true, Secrets: &secrets.Memory{},
			SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
				sent = true
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "send", "--draft-file", letter})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if sent {
			t.Fatal("dry-run sent mail")
		}
		validateOutput(t, "send.schema.json", out.Bytes())
		mustContain(t, out.String(), `"dry_run":true`, `"subject":"信件"`, `"body_summary":"你好"`)
	})

	t.Run("readonly beats draft-file execute", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "1")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		letter := writeLetter(t, "reader@example.com")
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--draft-file", letter, "--execute"})
		err := root.Execute()
		if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || !strings.Contains(err.Error(), "QQMAIL_CLI_READONLY") {
			t.Fatalf("readonly must refuse draft-file execute, got %v", err)
		}
	})

	t.Run("allowlist hit sends after TTY confirmation", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		letter := writeLetter(t, "reader@example.com")
		cachePath := filepath.Join(t.TempDir(), "cache.db")
		var out, stderr bytes.Buffer
		sent := false
		rt := &Runtime{
			Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader("send\n"),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return true },
			IndexOpen: func(_ string, write bool) (*index.DB, error) {
				return index.OpenPath(cachePath, write)
			},
			SendMail: func(_ context.Context, named account.Named, _ string, draft sendmail.Draft, _ []byte) error {
				sent = true
				if len(draft.To) != 1 || draft.To[0].Address != "reader@example.com" || draft.Subject != "信件" {
					t.Fatalf("unexpected draft: %+v", draft)
				}
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "send", "--draft-file", letter, "--execute"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if !sent {
			t.Fatal("transport was not called")
		}
		validateOutput(t, "send.schema.json", out.Bytes())
		if !strings.Contains(out.String(), `"sent":true`) {
			t.Fatalf("sent flag missing: %s", out.String())
		}
	})

	t.Run("allowlist miss refused", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		letter := writeLetter(t, "stranger@other.example")
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--draft-file", letter, "--execute"})
		err := root.Execute()
		if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
			t.Fatalf("allowlist miss must stay policy_denied, got %v", err)
		}
	})
}

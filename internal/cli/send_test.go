package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

const testAuthCode = "abcdefghijklmnop"

func TestSendExecuteRejectsEmptyAllowlistBeforeCredentialAndTransport(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, nil)
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
		Secrets: &secrets.Memory{}, IsTerminal: func(io.Reader) bool { return true },
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			sent = true
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture", "--execute"})
	err := root.Execute()
	if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || sent {
		t.Fatalf("empty allowlist was not blocked before transport: err=%v sent=%v", err, sent)
	}
}

func TestSendExecuteRejectsNonTTYBeforeCredentialAndTransport(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
		Secrets: &secrets.Memory{}, IsTerminal: func(io.Reader) bool { return false },
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			sent = true
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture", "--execute"})
	err := root.Execute()
	if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || sent {
		t.Fatalf("non-TTY send was not blocked before transport: err=%v sent=%v", err, sent)
	}
}

func TestSendExecuteUsesAllowlistAndWritesContentFreeAudit(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	cachePath := filepath.Join(t.TempDir(), "cache.db")
	var out, stderr bytes.Buffer
	sent := false
	rt := &Runtime{
		Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader("SEND\n"),
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		IsTerminal: func(io.Reader) bool { return true },
		IndexOpen: func(_ string, write bool) (*index.DB, error) {
			return index.OpenPath(cachePath, write)
		},
		SendMail: func(_ context.Context, named account.Named, authCode string, draft sendmail.Draft, raw []byte) error {
			sent = true
			if named.Email != "user@qq.com" || authCode != testAuthCode || len(raw) == 0 || len(draft.To) != 1 {
				t.Fatalf("unexpected transport input: account=%q auth-len=%d raw=%d recipients=%d", named.Email, len(authCode), len(raw), len(draft.To))
			}
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "send", "--to", "reader@example.com", "--subject", "fixture", "--body", "hello", "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !sent {
		t.Fatal("transport was not called")
	}
	validateOutput(t, "send.schema.json", out.Bytes())
	if strings.Contains(out.String()+stderr.String(), testAuthCode) {
		t.Fatal("credential leaked to command output")
	}
	store, err := index.OpenPath(cachePath, false)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.AuditList(context.Background(), 10)
	_ = store.Close()
	if err != nil || len(entries) != 2 || entries[0].Action != "send" || entries[1].Action != "send_attempt" {
		t.Fatalf("unexpected audit entries: %+v err=%v", entries, err)
	}
	rawAudit, err := os.ReadFile(cachePath + ".audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{testAuthCode, "reader@example.com", "fixture", "hello"} {
		if bytes.Contains(rawAudit, []byte(forbidden)) {
			t.Fatalf("message content leaked into audit JSONL: %q", forbidden)
		}
	}
}

func TestReplyAndForwardBuildExpectedThreading(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	id := "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"
	for _, tc := range []struct {
		name  string
		args  []string
		check func(*testing.T, sendmail.Draft)
	}{
		{
			name: "reply",
			args: []string{"reply", id, "--body", "thanks", "--execute"},
			check: func(t *testing.T, draft sendmail.Draft) {
				if len(draft.To) != 1 || draft.To[0].Address != "sender@example.com" || draft.Subject != "Re: fixture" || draft.InReplyTo != "fixture@example.com" || len(draft.References) != 1 || draft.References[0] != "fixture@example.com" || !strings.Contains(draft.Body, "> body") {
					t.Fatalf("unexpected reply draft: %+v", draft)
				}
			},
		},
		{
			name: "forward",
			args: []string{"forward", id, "--to", "reader@example.com", "--body", "FYI", "--execute"},
			check: func(t *testing.T, draft sendmail.Draft) {
				if len(draft.To) != 1 || draft.To[0].Address != "reader@example.com" || draft.Subject != "Fwd: fixture" || draft.InReplyTo != "" || len(draft.References) != 1 || draft.References[0] != "fixture@example.com" || !strings.Contains(draft.Body, "Forwarded message") {
					t.Fatalf("unexpected forward draft: %+v", draft)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := saveSendConfig(t, []string{"sender@example.com", "reader@example.com"})
			cachePath := filepath.Join(t.TempDir(), "cache.db")
			called := false
			rt := &Runtime{
				Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
				Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
				IsTerminal: func(io.Reader) bool { return true },
				Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
				IndexOpen: func(_ string, write bool) (*index.DB, error) {
					return index.OpenPath(cachePath, write)
				},
				SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, _ []byte) error {
					called = true
					tc.check(t, draft)
					return nil
				},
			}
			root := NewRoot(rt)
			root.SetArgs(append([]string{"--config", configPath}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("transport was not called")
			}
		})
	}
}

// relatedMailReader serves a multipart/related fixture (HTML body referencing
// a CID image plus an inline image/png leaf part) so forward can prove inline
// parts are re-attached instead of dropped.
type relatedMailReader struct{ fakeReader }

func (relatedMailReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: sender@example.com\r\n" +
		"Subject: fixture\r\n" +
		"Message-ID: <fixture@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=\"r1\"\r\n\r\n" +
		"--r1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>see the picture</p><img src=\"cid:image001\">\r\n" +
		"--r1\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"image001.png\"\r\nContent-ID: <image001>\r\nContent-Transfer-Encoding: base64\r\n\r\naW1hZ2UwMDE=\r\n" +
		"--r1--\r\n"), false, nil
}

// Forward must re-attach inline non-text parts (CID images) parsed from the
// original mail; before the mimeparse fix these were silently skipped and a
// forwarded mail lost its pictures. The envelope has_attachments stays
// hard-coded false (guard-encoded, see docs/compat/qq-20260902.md); the
// attachment list is the source of truth.
func TestForwardCarriesInlineImages(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	cachePath := filepath.Join(t.TempDir(), "cache.db")
	id := "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"
	called := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		IsTerminal: func(io.Reader) bool { return true },
		Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return relatedMailReader{}, nil },
		IndexOpen: func(_ string, write bool) (*index.DB, error) {
			return index.OpenPath(cachePath, write)
		},
		SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, _ []byte) error {
			called = true
			if len(draft.Attachments) != 1 {
				t.Fatalf("forward dropped inline images: %d attachments", len(draft.Attachments))
			}
			att := draft.Attachments[0]
			if att.Filename != "image001.png" || att.ContentType != "image/png" || len(att.Data) == 0 {
				t.Fatalf("unexpected forwarded attachment: %+v", att)
			}
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "forward", id, "--to", "reader@example.com", "--body", "FYI", "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("transport was not called")
	}
}

// originalAttachmentMailReader serves a mail carrying a single 1 KiB
// attachment, so a forward that also --attaches a file just under the 20 MiB
// cap crosses the combined limit only once the original attachments are
// appended.
type originalAttachmentMailReader struct{ fakeReader }

func (originalAttachmentMailReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: sender@example.com\r\n" +
		"Subject: fixture\r\n" +
		"Message-ID: <fixture@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"m1\"\r\n\r\n" +
		"--m1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--m1\r\nContent-Type: application/octet-stream; name=\"orig.bin\"\r\nContent-Disposition: attachment; filename=\"orig.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(make([]byte, 1024)) + "\r\n" +
		"--m1--\r\n"), false, nil
}

// Forward must fail the combined 20 MiB attachment cap with the loader's
// policy denial (exit 50), not validateDraft's generic build failure (exit 2):
// the original mail's attachments share the cap with --attach/--attach-inline.
func TestForwardCombinedAttachmentsOverCapIsPolicyDenied(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	bigFile := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(bigFile, make([]byte, int(sendmail.MaxTotalAttachmentBytes)-512), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return originalAttachmentMailReader{}, nil },
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "forward", "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ", "--to", "reader@example.com", "--body", "FYI", "--attach", bigFile})
	err := root.Execute()
	if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || !strings.Contains(err.Error(), "20 MiB") {
		t.Fatalf("forward over the combined cap must be policy_denied naming the cap, got %v", err)
	}
}

func saveSendConfig(t *testing.T, allowlist []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &account.Config{Schema: account.ConfigSchema, DefaultAccount: "personal", Accounts: map[string]account.Account{
		"personal": {Email: "user@qq.com", SendAllowlist: allowlist},
	}}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// --attach-inline wiring for send/reply/forward: the usage gate without html
// format, the dry-run preview with filename-derived Content-IDs, the 20 MiB cap
// shared with --attach, and the unchanged --execute TTY SEND gate.
func TestAttachInlineFlagWiring(t *testing.T) {
	pngMagic := []byte("\x89PNG\r\n\x1a\n")
	writeInlineFixture := func(t *testing.T, dir, name string, size int) string {
		t.Helper()
		data := append([]byte{}, pngMagic...)
		for len(data) < size {
			data = append(data, 0x41)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// An inline image is referenced by cid: from the HTML body, so a
	// plain-text body is a usage error (exit 2), not a silent attachment.
	t.Run("requires html body format", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, nil)
		inline := writeInlineFixture(t, t.TempDir(), "logo.png", len(pngMagic))
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture", "--body", "hi", "--attach-inline", inline})
		err := root.Execute()
		if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "html") {
			t.Fatalf("--attach-inline without --body-format html must be a usage error (exit %d) naming html, got %v", output.ExitUsage, err)
		}
	})

	// The dry-run preview must list every inline part with filename, type,
	// size and Content-ID; the Task 10 HTML warning line stays.
	t.Run("dry run lists inline parts with content ids", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		dir := t.TempDir()
		named := writeInlineFixture(t, dir, "logo.png", len(pngMagic))
		extensionless := writeInlineFixture(t, dir, "imageblob", len(pngMagic)) // sniffed by magic bytes
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture",
			"--body", `<p>见图 <img src="cid:a"></p>`, "--body-format", "html",
			"--attach-inline", named + "," + extensionless})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		human := out.String() + stderr.String()
		for _, want := range []string{
			"Inline: logo.png (image/png, 8 bytes, Content-ID: <logo.png@qq.com>)",
			"Inline: imageblob (image/png, 8 bytes, Content-ID: <imageblob@qq.com>)",
			"HTML 正文将原样发送，未经消毒；请检查上方源码摘要",
		} {
			if !strings.Contains(human, want) {
				t.Fatalf("dry-run preview missing %q\noutput:\n%s", want, human)
			}
		}
	})

	// The JSON summary carries the inline parts (with content_id) inside the
	// attachments list and stays schema-valid.
	t.Run("json summary carries inline content ids", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		inline := writeInlineFixture(t, t.TempDir(), "logo.png", len(pngMagic))
		var out, stderr bytes.Buffer
		rt := &Runtime{Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader(""), JSON: true, Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "send", "--to", "reader@example.com", "--subject", "fixture",
			"--body", "<p>x</p>", "--body-format", "html", "--attach-inline", inline})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "send.schema.json", out.Bytes())
		var envelope struct {
			Data struct {
				Summary struct {
					Attachments []struct {
						Filename  string `json:"filename"`
						ContentID string `json:"content_id"`
					} `json:"attachments"`
				} `json:"summary"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		attachments := envelope.Data.Summary.Attachments
		if len(attachments) != 1 || attachments[0].Filename != "logo.png" || attachments[0].ContentID != "logo.png@qq.com" {
			t.Fatalf("unexpected summary attachments: %+v", attachments)
		}
	})

	// --attach and --attach-inline share the 20 MiB combined cap (exit 50).
	t.Run("combined 20 MiB cap", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, nil)
		dir := t.TempDir()
		regular := writeInlineFixture(t, dir, "big.bin", 15<<20)
		inline := writeInlineFixture(t, dir, "big.png", 6<<20)
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture",
			"--body", "<p>x</p>", "--body-format", "html", "--attach", regular, "--attach-inline", inline})
		err := root.Execute()
		if err == nil {
			t.Fatal("21 MiB combined attachments were accepted")
		}
		failure, code := errmap.Details(err)
		if failure.Code != "policy_denied" || code != output.ExitPolicyDenied {
			t.Fatalf("21 MiB combined must be policy_denied (exit %d), got %v code=%d", output.ExitPolicyDenied, err, code)
		}
	})

	// Two --attach-inline files deriving the same bare Content-ID (same base
	// name in different directories) are a usage error, not a silent dedup:
	// the HTML body references images by file name, so a duplicate would leave
	// one cid: reference unresolvable on the receiving side.
	t.Run("duplicate inline filenames rejected", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, nil)
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		first := writeInlineFixture(t, dir, "logo.png", len(pngMagic))
		second := writeInlineFixture(t, sub, "logo.png", len(pngMagic))
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture",
			"--body", "<p>x</p>", "--body-format", "html", "--attach-inline", first, "--attach-inline", second})
		err := root.Execute()
		if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "logo.png@qq.com") {
			t.Fatalf("duplicate inline file names must be a usage error naming the derived id, got %v", err)
		}
	})

	// --execute with inlines present keeps both gate halves: non-TTY is
	// rejected before any transport, TTY + SEND reaches it with a
	// filename-derived bare Content-ID and a multipart/related wire format.
	t.Run("execute keeps TTY SEND gate", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		configPath := saveSendConfig(t, []string{"reader@example.com"})
		inline := writeInlineFixture(t, t.TempDir(), "logo.png", len(pngMagic))
		args := []string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture",
			"--body", `<p>x <img src="cid:a"></p>`, "--body-format", "html", "--attach-inline", inline, "--execute"}
		sent := false
		rt := &Runtime{
			Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return false },
			SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
				sent = true
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || sent {
			t.Fatalf("non-TTY execute with inlines must stay policy_denied: err=%v sent=%v", err, sent)
		}
		var rawMessage []byte
		rt2 := &Runtime{
			Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return true },
			IndexOpen: func(_ string, write bool) (*index.DB, error) {
				return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), write)
			},
			SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, raw []byte) error {
				rawMessage = raw
				if len(draft.Inlines) != 1 {
					t.Fatalf("expected 1 inline, got %d", len(draft.Inlines))
				}
				cid := draft.Inlines[0].ContentID
				if cid != "logo.png@qq.com" {
					t.Fatalf("content id not derived from filename: %q", cid)
				}
				if draft.Inlines[0].Filename != "logo.png" || draft.Inlines[0].ContentType != "image/png" {
					t.Fatalf("unexpected inline part: %+v", draft.Inlines[0])
				}
				return nil
			},
		}
		root2 := NewRoot(rt2)
		root2.SetArgs(args)
		if err := root2.Execute(); err != nil {
			t.Fatal(err)
		}
		if len(rawMessage) == 0 || !bytes.Contains(rawMessage, []byte("multipart/related")) {
			t.Fatalf("sent raw message is not multipart/related")
		}
		if !bytes.Contains(rawMessage, []byte("Content-Id: <logo.png@qq.com>")) {
			t.Fatalf("sent raw message missing the filename-derived Content-Id header")
		}
	})

	// reply and forward share the flag and must carry loaded inlines (with a
	// generated bare Content-ID) into the Draft they hand to the transport.
	t.Run("reply and forward accept attach-inline", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "0")
		id := "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"reply", []string{"reply", id, "--body", "see pic", "--body-format", "html", "--attach-inline", "INLINE", "--execute"}},
			{"forward", []string{"forward", id, "--to", "reader@example.com", "--body", "FYI", "--body-format", "html", "--attach-inline", "INLINE", "--execute"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				configPath := saveSendConfig(t, []string{"sender@example.com", "reader@example.com"})
				inline := writeInlineFixture(t, t.TempDir(), "logo.png", len(pngMagic))
				args := make([]string, 0, len(tc.args))
				for _, arg := range tc.args {
					if arg == "INLINE" {
						arg = inline
					}
					args = append(args, arg)
				}
				called := false
				rt := &Runtime{
					Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
					Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
					IsTerminal: func(io.Reader) bool { return true },
					Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
					IndexOpen: func(_ string, write bool) (*index.DB, error) {
						return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), write)
					},
					SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, _ []byte) error {
						called = true
						if draft.BodyFormat != "html" || len(draft.Inlines) != 1 || draft.Inlines[0].Filename != "logo.png" || draft.Inlines[0].ContentID != "logo.png@qq.com" {
							t.Fatalf("%s dropped --attach-inline: format=%q inlines=%+v", tc.name, draft.BodyFormat, draft.Inlines)
						}
						return nil
					},
				}
				root := NewRoot(rt)
				root.SetArgs(append([]string{"--config", configPath}, args...))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatal("transport was not called")
				}
			})
		}
	})
}

// The Content-ID must be derived deterministically from the file name so the
// cid:<filename@qq.com> reference in the HTML body resolves on the
// receiving side:
// spaces become dashes, control characters/angle brackets/'@'/whitespace are
// stripped, non-ASCII is kept, over-long names are truncated, and a name
// without usable characters is a usage error instead of an empty localpart.
func TestInlineContentIDDerivedFromFilename(t *testing.T) {
	t.Run("loader sets the derived bare id", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "logo.png")
		if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		inlines, err := loadInlineAttachments([]string{path}, "html", "qq.com", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(inlines) != 1 || inlines[0].ContentID != "logo.png@qq.com" || inlines[0].ContentType != "image/png" {
			t.Fatalf("unexpected inlines: %+v", inlines)
		}
	})
	t.Run("spaces become dashes", func(t *testing.T) {
		id, err := deriveInlineContentID("pic/my chart.png", "qq.com")
		if err != nil || id != "my-chart.png@qq.com" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	// A raw file name can carry bytes that would break the id@domain form:
	// control characters (e.g. \x01 from a bad archive) must be stripped, not
	// sent, and angle brackets/'@' would truncate or forge the header value.
	t.Run("control characters angle brackets and at are stripped", func(t *testing.T) {
		id, err := deriveInlineContentID("dir/de\x01ad<b>@v1.png", "qq.com")
		if err != nil || id != "deadbv1.png@qq.com" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("non-ascii preserved", func(t *testing.T) {
		id, err := deriveInlineContentID("报表/图表.png", "qq.com")
		if err != nil || id != "图表.png@qq.com" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("long filename truncated", func(t *testing.T) {
		id, err := deriveInlineContentID("pic/"+strings.Repeat("a", 200)+".png", "qq.com")
		want := strings.Repeat("a", 64) + "@qq.com"
		if err != nil || id != want {
			t.Fatalf("id length=%d err=%v, want 64-byte localpart", len(id), err)
		}
	})
	t.Run("filename without usable characters rejected", func(t *testing.T) {
		for _, path := range []string{"dir/<>@", "\t\t"} {
			id, err := deriveInlineContentID(path, "qq.com")
			if err == nil || errmap.Classify(err).Kind != errmap.Usage || id != "" {
				t.Fatalf("path %q: want usage error, got id=%q err=%v", path, id, err)
			}
		}
	})
}

// reply recipients come from the original mail (Reply-To/From). The --to flag
// exists on every compose command, so it must be rejected loudly instead of
// being silently ignored.
func TestReplyRejectsToFlagInsteadOfSilentlyIgnoring(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	id := "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"
	called := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		IsTerminal: func(io.Reader) bool { return true },
		Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			called = true
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "reply", id, "--to", "reader@example.com", "--body", "thanks", "--execute"})
	err := root.Execute()
	if err == nil || errmap.Classify(err).Kind != errmap.Usage || called {
		t.Fatalf("reply must reject --to with a usage error before any transport: err=%v called=%v", err, called)
	}
}

// The human dry-run preview for an HTML body must show the derived plain-text
// fallback and the escaped, truncated source excerpt plus the fixed warning —
// never the raw HTML source, and never an unescaped control character.
func TestSendDryRunHTMLPreviewShowsDerivedTextEscapedSourceAndWarning(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	var out, stderr bytes.Buffer
	rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
	root := NewRoot(rt)
	body := "<p>你好 <b>世界</b></p><script>alert('x')</script>\x1b[31mred\x1b[0m\x00" + strings.Repeat("A", 2500)
	root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture", "--body", body, "--body-format", "html"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	human := out.String() + stderr.String()
	for _, want := range []string{
		"你好 世界",
		"HTML 正文将原样发送，未经消毒；请检查上方源码摘要",
		"&#60;script&#62;",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("dry-run preview missing %q\noutput:\n%s", want, human)
		}
	}
	for _, bad := range []string{"\x1b", "\x00", "<script>"} {
		if strings.Contains(human, bad) {
			t.Fatalf("dry-run preview leaks unescaped %q\noutput:\n%s", bad, human)
		}
	}
}

// The JSON summary must carry the three HTML preview fields (only added, never
// removed) and stay valid against send.schema.json.
func TestSendDryRunJSONHTMLSummaryFields(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	var out, stderr bytes.Buffer
	rt := &Runtime{Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader(""), JSON: true, Secrets: &secrets.Memory{}}
	root := NewRoot(rt)
	body := "<p>你好 <b>世界</b></p>" + strings.Repeat("甲", 3000)
	root.SetArgs([]string{"--config", configPath, "--json", "send", "--to", "reader@example.com", "--subject", "fixture", "--body", body, "--body-format", "html"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	validateOutput(t, "send.schema.json", out.Bytes())
	var envelope struct {
		Data struct {
			Summary struct {
				BodyPreview       string `json:"body_preview"`
				HTMLSourceExcerpt string `json:"html_source_excerpt"`
				HTMLBytes         int    `json:"html_bytes"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	summary := envelope.Data.Summary
	if !strings.HasPrefix(summary.BodyPreview, "你好 世界") {
		t.Fatalf("body_preview = %q, want the derived plain text", summary.BodyPreview)
	}
	if summary.HTMLBytes != len(body) {
		t.Fatalf("html_bytes = %d, want %d", summary.HTMLBytes, len(body))
	}
	if summary.HTMLSourceExcerpt == "" || len(summary.HTMLSourceExcerpt) > 2048 {
		t.Fatalf("html_source_excerpt length = %d, want 1..2048", len(summary.HTMLSourceExcerpt))
	}
	if !strings.Contains(summary.HTMLSourceExcerpt, "&#60;b&#62;") {
		t.Fatalf("html_source_excerpt is not escaped: %q", summary.HTMLSourceExcerpt)
	}
}

// An unknown --body-format value is a usage error (exit 2), before anything is
// built or sent.
func TestSendRejectsUnknownBodyFormatWithUsageExitCode(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"reader@example.com"})
	rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &secrets.Memory{}}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "send", "--to", "reader@example.com", "--subject", "fixture", "--body", "hi", "--body-format", "rich"})
	err := root.Execute()
	// The rejection must name the offending value: validateBodyFormat rejects
	// it before draft assembly, and the Build-side whitelist stays as defense
	// in depth — not just any generic usage failure.
	if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "rich") {
		t.Fatalf("invalid --body-format must be a usage error (exit %d) naming the value, got %v", output.ExitUsage, err)
	}
}

// reply and forward share the flag and must reject an unknown --body-format
// before they even connect: the check runs before loadOriginal opens the
// IMAP connection.
func TestReplyAndForwardRejectUnknownBodyFormatBeforeConnecting(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"reply", []string{"reply", replyAllMsgID, "--body", "thanks", "--body-format", "rich"}},
		{"forward", []string{"forward", replyAllMsgID, "--to", "reader@example.com", "--body", "FYI", "--body-format", "rich"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := saveSendConfig(t, []string{"sender@example.com", "reader@example.com"})
			rt := &Runtime{
				Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
				Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
				Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
					t.Fatal("must not connect before the body-format check")
					return nil, nil
				},
			}
			root := NewRoot(rt)
			root.SetArgs(append([]string{"--config", configPath}, tc.args...))
			err := root.Execute()
			if err == nil || errmap.Classify(err).Kind != errmap.Usage || !strings.Contains(err.Error(), "rich") {
				t.Fatalf("invalid --body-format must be an early usage error naming the value, got %v", err)
			}
		})
	}
}

// reply and forward share the --body-format flag and must carry the value into
// the Draft they hand to the transport.
func TestReplyAndForwardCarryBodyFormat(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	id := "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"reply", []string{"reply", id, "--body", "thanks", "--body-format", "html", "--execute"}},
		{"forward", []string{"forward", id, "--to", "reader@example.com", "--body", "FYI", "--body-format", "html", "--execute"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := saveSendConfig(t, []string{"sender@example.com", "reader@example.com"})
			called := false
			rt := &Runtime{
				Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
				Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
				IsTerminal: func(io.Reader) bool { return true },
				Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
				SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, _ []byte) error {
					called = true
					if draft.BodyFormat != "html" {
						t.Fatalf("%s dropped --body-format: got %q", tc.name, draft.BodyFormat)
					}
					raw, err := sendmail.Build(draft)
					if err != nil {
						t.Fatalf("draft with html format does not build: %v", err)
					}
					if !bytes.Contains(raw, []byte("multipart/alternative")) {
						t.Fatalf("raw message has no multipart/alternative: %s", raw)
					}
					return nil
				},
			}
			root := NewRoot(rt)
			root.SetArgs(append([]string{"--config", configPath}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("transport was not called")
			}
		})
	}
}

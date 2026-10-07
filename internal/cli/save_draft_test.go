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
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

// saveDraftMutatorStub serves the drafts folder (\Drafts attribute on
// "Drafts") and records every APPEND the policy layer asks for. It doubles as
// the reader-side connection for the dry-run folder resolution.
type saveDraftMutatorStub struct {
	fakeReader
	appends []recordedAppend
}

type recordedAppend struct {
	folder string
	raw    []byte
}

func (s *saveDraftMutatorStub) Capabilities() ([]string, []string) {
	return []string{"IMAP4rev1"}, []string{"IMAP4rev1"}
}

func (s *saveDraftMutatorStub) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return []mailmodel.Folder{{Name: "Drafts", Attributes: []string{`\Drafts`}}}, nil
}

func (s *saveDraftMutatorStub) SetSeen(context.Context, mailmodel.MsgID) error { return nil }

func (s *saveDraftMutatorStub) SetFlags(context.Context, mailmodel.MsgID, []string, []string) error {
	return nil
}

func (s *saveDraftMutatorStub) CreateFolder(context.Context, string) error { return nil }

func (s *saveDraftMutatorStub) RenameFolder(context.Context, string, string) error { return nil }

func (s *saveDraftMutatorStub) AppendDraft(_ context.Context, folder string, raw []byte) error {
	s.appends = append(s.appends, recordedAppend{folder: folder, raw: raw})
	return nil
}

func (s *saveDraftMutatorStub) MoveUID(context.Context, mailmodel.MsgID, string) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *saveDraftMutatorStub) CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, imapx.MessageIdentity) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *saveDraftMutatorStub) LocateByIdentity(context.Context, string, imapx.MessageIdentity) ([]mailmodel.MsgID, error) {
	return nil, nil
}

// newSaveDraftRuntime wires a full send --save-draft runtime: the stub serves
// both dial directions, smtpCalls counts every transport invocation (the
// save-draft fork must keep it at zero), and terminal decides the TTY gate.
func newSaveDraftRuntime(t *testing.T, stub *saveDraftMutatorStub, smtpCalls *int, in string, terminal bool) (*Runtime, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(in),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			return stub, nil
		},
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return stub, nil
		},
		IndexOpen: func(string, bool) (*index.DB, error) {
			return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
		},
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			*smtpCalls++
			return nil
		},
		IsTerminal: func(io.Reader) bool { return terminal },
	}
	return rt, &out, &stderr
}

func runSaveDraftRoot(t *testing.T, rt *Runtime, configPath string, args ...string) error {
	t.Helper()
	root := NewRoot(rt)
	root.SetArgs(append([]string{"--config", configPath}, args...))
	return root.Execute()
}

func wantExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("command unexpectedly succeeded")
	}
	if _, code := errmap.Details(err); code != want {
		t.Fatalf("exit code %d, want %d: %v", code, want, err)
	}
}

func TestSaveDraftIsMutateGated(t *testing.T) {
	t.Run("readonly blocks before dial", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "1")
		stub := &saveDraftMutatorStub{}
		dialed := false
		var out, stderr bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: &stderr, In: strings.NewReader("1\n"),
			Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
				dialed = true
				return stub, nil
			},
			DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
				dialed = true
				return stub, nil
			},
			IsTerminal: func(io.Reader) bool { return true },
		}
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--save-draft", "--execute")
		wantExitCode(t, err, 50)
		if dialed {
			t.Fatal("readonly blocked command dialed the server")
		}
		if !strings.Contains(err.Error(), "QQMAIL_CLI_READONLY") {
			t.Fatalf("rejected by a non-readonly gate: %v", err)
		}
	})

	t.Run("dry-run reports without executing", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		stub := &saveDraftMutatorStub{}
		out := runCLIReader(t, stub, "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--save-draft", "--json")
		mustContain(t, out, `"action":"save_draft"`, `"destination":"Drafts"`, `"dry_run":true`)
		if len(stub.appends) != 0 {
			t.Fatalf("dry-run appended to the server: %+v", stub.appends)
		}
	})

	t.Run("dry-run human preview matches the send draft", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		stub := &saveDraftMutatorStub{}
		out := runCLIReader(t, stub, "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--save-draft")
		mustContain(t, out, "To: <allowed@example.com>", "Subject: s", "Body: b", "DRY RUN")
	})

	t.Run("execute requires TTY confirmation", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		stub := &saveDraftMutatorStub{}
		smtpCalls := 0
		rt, _, _ := newSaveDraftRuntime(t, stub, &smtpCalls, "", false)
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--save-draft", "--execute")
		wantExitCode(t, err, 50)
		if len(stub.appends) != 0 || smtpCalls != 0 {
			t.Fatalf("unconfirmed execute mutated: appends=%d smtp=%d", len(stub.appends), smtpCalls)
		}
	})

	t.Run("execute appends to drafts and never dials SMTP", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		stub := &saveDraftMutatorStub{}
		smtpCalls := 0
		rt, out, stderr := newSaveDraftRuntime(t, stub, &smtpCalls, "1\n", true)
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		if err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--save-draft", "--execute"); err != nil {
			t.Fatalf("execute failed: %v (stderr=%s)", err, stderr.String())
		}
		mustContain(t, out.String(), `"action":"save_draft"`, `"destination":"Drafts"`, `"completed":1`, `"sent":false`)
		if len(stub.appends) != 1 || stub.appends[0].folder != "Drafts" {
			t.Fatalf("appends: %+v", stub.appends)
		}
		if !strings.Contains(string(stub.appends[0].raw), "Subject: s") {
			t.Fatalf("appended raw is not the built draft: %q", stub.appends[0].raw)
		}
		if smtpCalls != 0 {
			t.Fatalf("SMTP transport was called %d times", smtpCalls)
		}
		mustContain(t, stderr.String(), "To: <allowed@example.com>", "Subject: s", "将把草稿存入服务器草稿箱 Drafts（不发送）")
	})

	t.Run("allowlist does not apply to save-draft", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		stub := &saveDraftMutatorStub{}
		smtpCalls := 0
		rt, out, stderr := newSaveDraftRuntime(t, stub, &smtpCalls, "1\n", true)
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		if err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "nobody@stranger.example", "--subject", "s", "--body", "b", "--save-draft", "--execute"); err != nil {
			t.Fatalf("stranger-recipient save-draft failed: %v (stderr=%s)", err, stderr.String())
		}
		mustContain(t, out.String(), `"action":"save_draft"`, `"completed":1`)
		if len(stub.appends) != 1 || smtpCalls != 0 {
			t.Fatalf("appends=%d smtp=%d", len(stub.appends), smtpCalls)
		}
	})
}

// --save-draft is a flag on the shared compose fork, so the attachment paths
// must survive it unchanged: --attach-inline keeps multipart/related with
// Content-IDs, --attach keeps multipart/mixed, and neither ever reaches SMTP.
func TestSaveDraftCombinesWithAttachments(t *testing.T) {
	t.Run("attach-inline keeps multipart/related", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		png := filepath.Join(t.TempDir(), "logo.png")
		if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stub := &saveDraftMutatorStub{}
		smtpCalls := 0
		rt, _, stderr := newSaveDraftRuntime(t, stub, &smtpCalls, "1\n", true)
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		if err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "s", "--body-format", "html", "--body", `<p>hi</p><img src="cid:logo.png@qq.com">`, "--attach-inline", png, "--save-draft", "--execute"); err != nil {
			t.Fatalf("attach-inline save-draft failed: %v (stderr=%s)", err, stderr.String())
		}
		if len(stub.appends) != 1 {
			t.Fatalf("appends: %+v", stub.appends)
		}
		raw := strings.ToLower(string(stub.appends[0].raw))
		if !strings.Contains(raw, "multipart/related") || !strings.Contains(raw, "content-id") || !strings.Contains(raw, "logo.png") {
			t.Fatalf("inline draft lost related structure: %q", raw)
		}
		if smtpCalls != 0 {
			t.Fatalf("SMTP transport was called %d times", smtpCalls)
		}
	})

	t.Run("attach keeps multipart/mixed", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		bin := filepath.Join(t.TempDir(), "data.bin")
		if err := os.WriteFile(bin, []byte("\x00\x01\x02"), 0o600); err != nil {
			t.Fatal(err)
		}
		stub := &saveDraftMutatorStub{}
		smtpCalls := 0
		rt, _, stderr := newSaveDraftRuntime(t, stub, &smtpCalls, "1\n", true)
		configPath := saveSendConfig(t, []string{"allowed@example.com"})
		if err := runSaveDraftRoot(t, rt, configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "s", "--body", "b", "--attach", bin, "--save-draft", "--execute"); err != nil {
			t.Fatalf("attach save-draft failed: %v (stderr=%s)", err, stderr.String())
		}
		if len(stub.appends) != 1 {
			t.Fatalf("appends: %+v", stub.appends)
		}
		raw := string(stub.appends[0].raw)
		if !strings.Contains(raw, "multipart/mixed") || !strings.Contains(raw, "data.bin") {
			t.Fatalf("attached draft lost mixed structure: %q", raw)
		}
		if smtpCalls != 0 {
			t.Fatalf("SMTP transport was called %d times", smtpCalls)
		}
	})
}

// reply and forward share the same runDraft fork as send, so a plain dry-run
// must report the drafts destination without any SMTP or append traffic.
func TestSaveDraftAppliesToReplyAndForward(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")
	stub := &saveDraftMutatorStub{}
	out := runCLIReader(t, stub, "reply", inboxIDString, "--body", "thanks", "--save-draft", "--json")
	mustContain(t, out, `"action":"save_draft"`, `"destination":"Drafts"`)
	out = runCLIReader(t, stub, "forward", inboxIDString, "--to", "reader@example.com", "--body", "FYI", "--save-draft", "--json")
	mustContain(t, out, `"action":"save_draft"`, `"destination":"Drafts"`)
	if len(stub.appends) != 0 {
		t.Fatalf("dry-run appended to the server: %+v", stub.appends)
	}
}

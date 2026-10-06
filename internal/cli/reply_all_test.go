package cli

import (
	"bytes"
	"context"
	"io"
	"net/mail"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

const replyAllMsgID = "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"

func TestMergeReplyAll(t *testing.T) {
	self := "me@qq.com"
	m := func(s string) mail.Address { return mail.Address{Address: s} }
	to, cc := mergeReplyAll(self,
		[]mail.Address{m("boss@example.com")},
		[]mail.Address{m("Boss@Example.com"), m("me@qq.com"), m("a@example.com")},
		[]mail.Address{m("a@example.com"), m("b@example.com")},
	)
	wantTo := []mail.Address{m("boss@example.com"), m("a@example.com")}
	wantCc := []mail.Address{m("b@example.com")}
	if !reflect.DeepEqual(to, wantTo) || !reflect.DeepEqual(cc, wantCc) {
		t.Fatalf("to=%v cc=%v", to, cc)
	}
}

func TestMergeReplyAllNeverIncludesSelf(t *testing.T) {
	// self 出现在 replyTo/origTo/origCc 任意位置都被剔除（大小写不敏感）；
	// 大小写/空白变体在 To 与 Cc 之间也只保留首个。
	self := "Me@QQ.com"
	m := func(s string) mail.Address { return mail.Address{Address: s} }
	to, cc := mergeReplyAll(self,
		[]mail.Address{m("me@qq.com"), m("X@Example.com")},
		[]mail.Address{m("ME@qq.com"), m("x@example.com"), m("y@example.com")},
		[]mail.Address{m("me@QQ.com"), m("y@example.com"), m(" Y@Example.com"), m("z@example.com")},
	)
	wantTo := []mail.Address{m("X@Example.com"), m("y@example.com")}
	wantCc := []mail.Address{m("z@example.com")}
	if !reflect.DeepEqual(to, wantTo) || !reflect.DeepEqual(cc, wantCc) {
		t.Fatalf("to=%v cc=%v", to, cc)
	}
	for _, list := range [][]mail.Address{to, cc} {
		for _, address := range list {
			if strings.EqualFold(strings.TrimSpace(address.Address), "me@qq.com") {
				t.Fatalf("self leaked into reply-all recipients: %v", list)
			}
		}
	}
}

// replyAllMailReader serves a fixture whose To/Cc carry a case variant, the
// account itself and duplicates, plus a Bcc header, so reply-all merging and
// the "original Bcc never propagates" invariant can be proven end to end.
type replyAllMailReader struct{ fakeReader }

func (replyAllMailReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: Boss <boss@example.com>\r\n" +
		"To: Boss@Example.com, user@qq.com, a@example.com\r\n" +
		"Cc: a@example.com, b@example.com\r\n" +
		"Bcc: secret@example.com\r\n" +
		"Subject: fixture\r\n" +
		"Message-ID: <fixture@example.com>\r\n\r\nbody\r\n"), false, nil
}

// selfSentMailReader serves a mail whose sender and only recipient are the
// account itself, so --reply-all merges down to zero recipients.
type selfSentMailReader struct{ fakeReader }

func (selfSentMailReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: user@qq.com\r\nTo: user@qq.com\r\nSubject: note to self\r\nMessage-ID: <self@example.com>\r\n\r\nbody\r\n"), false, nil
}

// brokenAddressMailReader serves a mail whose To/Cc headers cannot be parsed
// as address lists; loadOriginal must treat them as empty, record warnings and
// never block the reply.
type brokenAddressMailReader struct{ fakeReader }

func (brokenAddressMailReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: boss@example.com\r\n" +
		"To: not-an-address\r\n" +
		"Cc: @@also-broken@@\r\n" +
		"Subject: fixture\r\n" +
		"Message-ID: <fixture@example.com>\r\n\r\nbody\r\n"), false, nil
}

func newReplyAllRuntime(t *testing.T, configPath string, reader imapx.Reader, onSend func(sendmail.Draft)) (*Runtime, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	rt := &Runtime{
		Out: out, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		IsTerminal: func(io.Reader) bool { return true },
		Dial:       func(context.Context, account.Named, string) (imapx.Reader, error) { return reader, nil },
		IndexOpen: func(_ string, write bool) (*index.DB, error) {
			return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), write)
		},
		SendMail: func(_ context.Context, _ account.Named, _ string, draft sendmail.Draft, _ []byte) error {
			onSend(draft)
			return nil
		},
	}
	return rt, out
}

// assertBccNeverPropagates is the defensive invariant: the original mail's Bcc
// header is not part of the received envelope and must never reach any
// recipient field of the draft — mergeReplyAll does not even take a Bcc
// parameter, and this test proves the wiring keeps it that way end to end.
// Flag-provided --bcc recipients remain legitimate draft Bcc.
func assertBccNeverPropagates(t *testing.T, draft sendmail.Draft) {
	t.Helper()
	for _, list := range [][]mail.Address{draft.To, draft.Cc, draft.Bcc} {
		for _, address := range list {
			if strings.EqualFold(address.Address, "secret@example.com") {
				t.Fatalf("original Bcc recipient leaked into reply recipients: %v", list)
			}
		}
	}
}

func TestReplyAllMergesOriginalToAndCc(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"boss@example.com", "a@example.com", "b@example.com"})
	var captured sendmail.Draft
	rt, _ := newReplyAllRuntime(t, configPath, replyAllMailReader{}, func(draft sendmail.Draft) { captured = draft })
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "reply", replyAllMsgID, "--reply-all", "--body", "thanks", "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	// Sender (From fallback, no Reply-To) first, original To minus self next;
	// the case-variant Boss@Example.com dedupes away and self is dropped.
	if len(captured.To) != 2 || captured.To[0].Address != "boss@example.com" || captured.To[1].Address != "a@example.com" {
		t.Fatalf("reply-all To mismatch: %+v", captured.To)
	}
	if len(captured.Cc) != 1 || captured.Cc[0].Address != "b@example.com" {
		t.Fatalf("reply-all Cc mismatch: %+v", captured.Cc)
	}
	if len(captured.Bcc) != 0 {
		t.Fatalf("original Bcc leaked into draft.Bcc without --bcc: %+v", captured.Bcc)
	}
	assertBccNeverPropagates(t, captured)
}

func TestReplyAllAppendsCcAndBccFlags(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"boss@example.com", "a@example.com", "b@example.com", "extra@example.com", "hidden@example.com"})
	var captured sendmail.Draft
	rt, _ := newReplyAllRuntime(t, configPath, replyAllMailReader{}, func(draft sendmail.Draft) { captured = draft })
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "reply", replyAllMsgID, "--reply-all", "--cc", "extra@example.com", "--bcc", "hidden@example.com", "--body", "thanks", "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(captured.Cc) != 2 || captured.Cc[0].Address != "b@example.com" || captured.Cc[1].Address != "extra@example.com" {
		t.Fatalf("--cc must append after the merged original Cc: %+v", captured.Cc)
	}
	if len(captured.Bcc) != 1 || captured.Bcc[0].Address != "hidden@example.com" {
		t.Fatalf("--bcc must stay the only Bcc source: %+v", captured.Bcc)
	}
	assertBccNeverPropagates(t, captured)
}

func TestReplyAllDryRunPreviewShowsExpandedRecipients(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"boss@example.com", "a@example.com", "b@example.com"})
	called := false
	rt, out := newReplyAllRuntime(t, configPath, replyAllMailReader{}, func(sendmail.Draft) { called = true })
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "reply", replyAllMsgID, "--reply-all", "--body", "thanks"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"To:", "Cc:", "boss@example.com", "a@example.com", "b@example.com", "DRY RUN"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run preview must show expanded recipients (%q missing):\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "secret@example.com") {
		t.Fatalf("dry-run preview leaked the original Bcc recipient:\n%s", out.String())
	}
	if called {
		t.Fatal("dry-run must not reach the transport")
	}
}

func TestReplyAllEmptyRecipientsIsUsageError(t *testing.T) {
	// 原邮件只有自己 → --reply-all 合并后 To/Cc 皆空 → usage error
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath := saveSendConfig(t, []string{"user@qq.com"})
	called := false
	rt, _ := newReplyAllRuntime(t, configPath, selfSentMailReader{}, func(sendmail.Draft) { called = true })
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "reply", replyAllMsgID, "--reply-all", "--body", "hi", "--execute"})
	err := root.Execute()
	if err == nil || errmap.Classify(err).Kind != errmap.Usage || called {
		t.Fatalf("reply-all with no surviving recipients must fail as a usage error before transport: err=%v called=%v", err, called)
	}
}

func TestLoadOriginalToleratesUnparsableToAndCc(t *testing.T) {
	configPath := saveSendConfig(t, nil)
	id, err := mailmodel.ParseMsgID(replyAllMsgID)
	if err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
		ConfigPath: configPath,
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			return brokenAddressMailReader{}, nil
		},
	}
	original, err := loadOriginal(rt, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(original.To) != 0 || len(original.Cc) != 0 {
		t.Fatalf("unparsable To/Cc must be treated as empty: to=%v cc=%v", original.To, original.Cc)
	}
	if len(original.Warnings) != 2 {
		t.Fatalf("expected one warning per unparsable header, got %v", original.Warnings)
	}
}

// reply and forward must surface the original mail's unparsable recipient
// headers on stderr instead of silently dropping them: the compose output has
// no warnings field, so stderr is the only presentation surface.
func TestComposeSurfacesOriginalHeaderWarningsOnStderr(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"reply", []string{"reply", replyAllMsgID, "--body", "thanks", "--execute"}},
		{"forward", []string{"forward", replyAllMsgID, "--to", "reader@example.com", "--body", "FYI"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := saveSendConfig(t, []string{"boss@example.com", "reader@example.com"})
			rt, _ := newReplyAllRuntime(t, configPath, brokenAddressMailReader{}, func(sendmail.Draft) {})
			root := NewRoot(rt)
			root.SetArgs(append([]string{"--config", configPath}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			stderr := rt.Err.(*bytes.Buffer).String()
			if !strings.Contains(stderr, "2 个收件人头无法解析") || !strings.Contains(stderr, "To 头解析失败") {
				t.Fatalf("%s must surface the original header warnings on stderr:\n%s", tc.name, stderr)
			}
		})
	}
}

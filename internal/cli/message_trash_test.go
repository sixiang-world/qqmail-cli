package cli

import (
	"bytes"
	"context"
	"io"
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
)

var (
	// trashID embeds the server-resolved trash folder: the id itself claims to
	// already live in the trash, so message trash must refuse it as a usage
	// error instead of moving a message onto itself.
	trashID       = mailmodel.MsgID{Folder: "Deleted Messages", UIDValidity: 1, UID: 1}
	trashIDString = trashID.String()
	inboxID       = mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 1}
	inboxIDString = inboxID.String()
)

// trashReader fakes the QQ Mail server dialect: LIST reports "Deleted
// Messages" carrying the \Trash special-use attribute, which
// cleaner.TrashFolder resolves by attribute before any name candidate. The
// command must never guess the folder name — it can only use what LIST said.
type trashReader struct {
	fakeReader
}

func (trashReader) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return []mailmodel.Folder{{Name: "Deleted Messages", Attributes: []string{`\Trash`}}}, nil
}

// trashMutatorStub serves the reader surface fetchMessageIdentity needs and
// records every move destination policy.Service asks for. Capabilities
// advertise MOVE so the service takes the uid-move path.
type trashMutatorStub struct {
	trashReader
	envelopesMissing bool
	moves            []string
}

func (s *trashMutatorStub) Capabilities() ([]string, []string) {
	return []string{"IMAP4rev1"}, []string{"IMAP4rev1", "MOVE"}
}

func (s *trashMutatorStub) Examine(context.Context, string) (uint32, uint32, error) {
	return 1, 1, nil
}

func (s *trashMutatorStub) FetchEnvelopes(_ context.Context, folder string, uidValidity uint32, ids []uint32) ([]mailmodel.Envelope, error) {
	if s.envelopesMissing {
		return []mailmodel.Envelope{}, nil
	}
	envelopes := []mailmodel.Envelope{}
	for _, uid := range ids {
		envelopes = append(envelopes, mailmodel.Envelope{UID: uid, Folder: folder, UIDValidity: uidValidity, Size: 42})
	}
	return envelopes, nil
}

func (s *trashMutatorStub) FetchHeaderFields(_ context.Context, ids []uint32) ([]mailmodel.HeaderFields, error) {
	headers := []mailmodel.HeaderFields{}
	for _, uid := range ids {
		headers = append(headers, mailmodel.HeaderFields{UID: uid, MessageID: "<fixture@example.com>"})
	}
	return headers, nil
}

func (s *trashMutatorStub) SetSeen(context.Context, mailmodel.MsgID) error { return nil }

func (s *trashMutatorStub) SetFlags(context.Context, mailmodel.MsgID, []string, []string) error {
	return nil
}

func (s *trashMutatorStub) CreateFolder(context.Context, string) error { return nil }

func (s *trashMutatorStub) RenameFolder(context.Context, string, string) error { return nil }

func (s *trashMutatorStub) MoveUID(_ context.Context, id mailmodel.MsgID, destination string) (imapx.MutationResult, error) {
	s.moves = append(s.moves, id.String()+"->"+destination)
	return imapx.MutationResult{Method: "uid_move", Destination: destination, DestinationVerified: true}, nil
}

func (s *trashMutatorStub) CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, imapx.MessageIdentity) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *trashMutatorStub) LocateByIdentity(context.Context, string, imapx.MessageIdentity) ([]mailmodel.MsgID, error) {
	return nil, nil
}

type trashFixture struct {
	Reader trashReader
	Stub   *trashMutatorStub
}

func newTrashFixture(t *testing.T) *trashFixture {
	t.Helper()
	t.Setenv(policy.ReadonlyEnv, "0")
	return &trashFixture{Reader: trashReader{}, Stub: &trashMutatorStub{}}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTrashResolvesDestinationAndRefusesTrashFolder(t *testing.T) {
	f := newTrashFixture(t) // LIST 返回 \Trash 属性文件夹 "Deleted Messages"
	// 已在回收站的 id（id.Folder == "Deleted Messages"）→ 退出码 2，错误信息说明已在回收站
	_, err := tryRunCLI(t, f.Reader, []string{"message", "trash", trashIDString, "--json"})
	if err == nil {
		t.Fatal("message trash on a message already inside the trash folder unexpectedly succeeded")
	}
	failure, code := errmap.Details(err)
	if code != 2 || !strings.Contains(failure.Message, "已在回收站") {
		t.Fatalf("exit code %d, message %q; want exit 2 with 已在回收站", code, failure.Message)
	}
	// 正常路径 dry-run：extra 含 {"action":"trash","destination":"Deleted Messages"}
	out := runCLIReader(t, f.Reader, "message", "trash", inboxIDString, "--json")
	mustContain(t, out, `"action":"trash"`, `"destination":"Deleted Messages"`)
	// --execute + TTY 确认后：逐封走 Service.Move，与 message move 的成功/失败分支一致
	var outBuf, stderr bytes.Buffer
	configPath := saveSendConfig(t, nil)
	rt := &Runtime{
		Out: &outBuf, Err: &stderr, In: strings.NewReader("1\n"),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return f.Stub, nil
		},
		IndexOpen: func(string, bool) (*index.DB, error) {
			return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
		},
		IsTerminal: func(io.Reader) bool { return true },
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "message", "trash", inboxIDString, "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatalf("message trash execute failed: %v (stderr=%s)", err, stderr.String())
	}
	mustContain(t, outBuf.String(), `"action":"trash"`, `"destination":"Deleted Messages"`, `"completed":1`)
	if len(f.Stub.moves) != 1 || !strings.HasSuffix(f.Stub.moves[0], "->Deleted Messages") {
		t.Fatalf("unexpected move calls: %v", f.Stub.moves)
	}
	// 失败分支与 move 一致：单封身份读取失败降级为 warning，批结果为部分成功。
	missing := &trashMutatorStub{envelopesMissing: true}
	var missOut, missErr bytes.Buffer
	rt = &Runtime{
		Out: &missOut, Err: &missErr, In: strings.NewReader("1\n"),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return missing, nil
		},
		IndexOpen: func(string, bool) (*index.DB, error) {
			return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
		},
		IsTerminal: func(io.Reader) bool { return true },
	}
	root = NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "message", "trash", inboxIDString, "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatalf("message trash execute (missing) failed: %v (stderr=%s)", err, missErr.String())
	}
	mustContain(t, missOut.String(), `"completed":0`, "邮件不存在")
	if len(missing.moves) != 0 {
		t.Fatalf("missing message must not reach MoveUID: %v", missing.moves)
	}
}

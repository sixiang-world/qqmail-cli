package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/situker/qqmail-cli/internal/secrets"
)

// folderMutatorStub records the folder-structure mutations policy.Service asks
// for; the read half is inherited from fakeReader.
type folderMutatorStub struct {
	fakeReader
	created []string
	renamed []string
}

func (s *folderMutatorStub) SetSeen(context.Context, mailmodel.MsgID) error { return nil }

func (s *folderMutatorStub) SetFlags(context.Context, mailmodel.MsgID, []string, []string) error {
	return nil
}

func (s *folderMutatorStub) CreateFolder(_ context.Context, name string) error {
	s.created = append(s.created, name)
	return nil
}

func (s *folderMutatorStub) RenameFolder(_ context.Context, oldName, newName string) error {
	s.renamed = append(s.renamed, oldName+"->"+newName)
	return nil
}

func (s *folderMutatorStub) MoveUID(context.Context, mailmodel.MsgID, string) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *folderMutatorStub) CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, imapx.MessageIdentity) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *folderMutatorStub) LocateByIdentity(context.Context, string, imapx.MessageIdentity) ([]mailmodel.MsgID, error) {
	return nil, nil
}

func TestFolderMutateGates(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "1") // readonly：--execute 一律被拒，dry-run 不受影响
	// INBOX 守卫：create 的名字、rename 的 old 与 new 都查（RENAME INBOX 会移动全部邮件）
	runExit(t, 2, "folder", "create", "INBOX", "--json")
	runExit(t, 2, "folder", "rename", "INBOX", "x", "--json")
	runExit(t, 2, "folder", "rename", "x", "INBOX", "--json")
	// readonly + --execute → 退出码 50（照 cli_test.go 既有矩阵语义：无 --execute 时 dry-run 退出 0）
	runExit(t, 50, "folder", "create", "arch/2026", "--execute", "--json")
	runExit(t, 50, "folder", "rename", "arch/2026", "arch/2027", "--execute", "--json")
	// dry-run：extra {"action":"folder_create","name":...} / {"action":"folder_rename","old":...,"new":...}
	out := runCLI(t, "folder", "create", "arch/2026", "--json")
	mustContain(t, out, `"action":"folder_create"`, `"name":"arch/2026"`, `"dry_run":true`)
	out = runCLI(t, "folder", "rename", "arch/2026", "arch/2027", "--json")
	mustContain(t, out, `"action":"folder_rename"`, `"old":"arch/2026"`, `"new":"arch/2027"`, `"dry_run":true`)
}

func TestFolderCreateAndRenameExecute(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")
	configPath := saveSendConfig(t, nil)
	runFolderMutate := func(stub *folderMutatorStub, in string, args ...string) (string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: &stderr, In: strings.NewReader(in),
			Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
				return stub, nil
			},
			IndexOpen: func(string, bool) (*index.DB, error) {
				return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
			},
			IsTerminal: func(io.Reader) bool { return in != "" },
		}
		root := NewRoot(rt)
		root.SetArgs(append([]string{"--config", configPath, "--json"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("folder mutate execute failed: %v (stderr=%s)", err, stderr.String())
		}
		return out.String(), stderr.String()
	}
	// --execute + TTY 输入 "1" 确认 → 创建成功并写审计
	stub := &folderMutatorStub{}
	out, stderr := runFolderMutate(stub, "1\n", "folder", "create", "arch/2026", "--execute")
	mustContain(t, out, `"action":"folder_create"`, `"name":"arch/2026"`, `"completed":1`)
	if !strings.Contains(stderr, "请输入") {
		t.Fatalf("confirmation prompt missing: %s", stderr)
	}
	if len(stub.created) != 1 || stub.created[0] != "arch/2026" || len(stub.renamed) != 0 {
		t.Fatalf("unexpected create calls: created=%v renamed=%v", stub.created, stub.renamed)
	}
	// rename 同构
	stub = &folderMutatorStub{}
	out, _ = runFolderMutate(stub, "1\n", "folder", "rename", "arch/2026", "arch/2027", "--execute")
	mustContain(t, out, `"action":"folder_rename"`, `"old":"arch/2026"`, `"new":"arch/2027"`, `"completed":1`)
	if len(stub.renamed) != 1 || stub.renamed[0] != "arch/2026->arch/2027" || len(stub.created) != 0 {
		t.Fatalf("unexpected rename calls: created=%v renamed=%v", stub.created, stub.renamed)
	}
	// 非 TTY（IsTerminal=false）在 readonly 关闭时同样被拒：退出码 50
	runExit(t, 50, "folder", "create", "arch/2026", "--execute", "--json")
}

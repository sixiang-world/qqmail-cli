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
	"github.com/situker/qqmail-cli/internal/policy"
	"github.com/situker/qqmail-cli/internal/secrets"
)

// The five v0.4 mutation commands carry dedicated schemas. Every real --json
// output they produce — the dry-run shape and the executed shape — must
// validate against the embedded schema: a schema that has drifted from real
// output is a broken contract for every agent consumer, and no unit test on
// individual fields would notice.
func TestMutationCommandsMatchSchemas(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")
	configPath := saveSendConfig(t, nil)
	cases := []struct {
		name     string
		args     []string
		schema   string
		executes bool // true: an --execute row; the output must prove a real execution, not a dry-run
		reader   imapx.Reader // serves the reader-side dial; trash resolves its destination through it
		mutator  imapx.Mutator
	}{
		// Dry-run shapes: no confirmation, no write connection.
		{name: "mark-unread dry-run", args: []string{"message", "mark-unread", inboxIDString}, schema: "message.mark-unread.schema.json", mutator: &policyMutatorStub{}},
		{name: "flag dry-run", args: []string{"message", "flag", inboxIDString, "--add", "\\Flagged"}, schema: "message.flag.schema.json", mutator: &policyMutatorStub{}},
		{name: "trash dry-run", args: []string{"message", "trash", inboxIDString}, schema: "message.trash.schema.json", reader: trashReader{}, mutator: &trashMutatorStub{}},
		{name: "folder create dry-run", args: []string{"folder", "create", "arch/2026"}, schema: "folder.create.schema.json", mutator: &folderMutatorStub{}},
		{name: "folder rename dry-run", args: []string{"folder", "rename", "arch/2026", "arch/2027"}, schema: "folder.rename.schema.json", mutator: &folderMutatorStub{}},
		// Executed shapes: TTY "1" confirms the one requested message/folder.
		{name: "mark-unread execute", args: []string{"message", "mark-unread", inboxIDString, "--execute"}, schema: "message.mark-unread.schema.json", executes: true, mutator: &policyMutatorStub{}},
		{name: "flag execute", args: []string{"message", "flag", inboxIDString, "--add", "\\Flagged", "--execute"}, schema: "message.flag.schema.json", executes: true, mutator: &policyMutatorStub{}},
		{name: "trash execute", args: []string{"message", "trash", inboxIDString, "--execute"}, schema: "message.trash.schema.json", executes: true, reader: trashReader{}, mutator: &trashMutatorStub{}},
		{name: "folder create execute", args: []string{"folder", "create", "arch/2026", "--execute"}, schema: "folder.create.schema.json", executes: true, mutator: &folderMutatorStub{}},
		{name: "folder rename execute", args: []string{"folder", "rename", "arch/2026", "arch/2027", "--execute"}, schema: "folder.rename.schema.json", executes: true, mutator: &folderMutatorStub{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader
			if reader == nil {
				reader = fakeReader{}
			}
			var out, stderr bytes.Buffer
			rt := &Runtime{
				Out: &out, Err: &stderr, In: strings.NewReader("1\n"),
				Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
				Dial: func(context.Context, account.Named, string) (imapx.Reader, error) { return reader, nil },
				DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) { return tc.mutator, nil },
				IndexOpen: func(string, bool) (*index.DB, error) {
					return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
				},
				IsTerminal: func(io.Reader) bool { return true },
			}
			root := NewRoot(rt)
			root.SetArgs(append([]string{"--config", configPath, "--json"}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatalf("%s failed: %v (stderr=%s)", tc.name, err, stderr.String())
			}
			// The schemas leave dry_run/execute unpinned, so a mislabeled row
			// would validate the wrong shape silently — assert the state.
			if tc.executes {
				mustContain(t, out.String(), `"execute":true`)
			} else {
				mustContain(t, out.String(), `"dry_run":true`)
			}
			validateOutput(t, tc.schema, out.Bytes())
		})
	}
}

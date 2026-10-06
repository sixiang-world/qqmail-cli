package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/policy"
)

// policyMutatorStub records what the policy layer asks the IMAP mutator to do.
// The read half is inherited from fakeReader; mark-unread must reach SetFlags
// with an empty add list and exactly the \\Seen removal.
type policyMutatorStub struct {
	fakeReader
	LastSetFlags struct {
		add    []string
		remove []string
	}
}

func (s *policyMutatorStub) SetSeen(context.Context, mailmodel.MsgID) error { return nil }

func (s *policyMutatorStub) SetFlags(_ context.Context, _ mailmodel.MsgID, add, remove []string) error {
	s.LastSetFlags.add = add
	s.LastSetFlags.remove = remove
	return nil
}

func (s *policyMutatorStub) CreateFolder(context.Context, string) error { return nil }

func (s *policyMutatorStub) RenameFolder(context.Context, string, string) error { return nil }

func (s *policyMutatorStub) MoveUID(context.Context, mailmodel.MsgID, string) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *policyMutatorStub) CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, imapx.MessageIdentity) (imapx.MutationResult, error) {
	return imapx.MutationResult{}, nil
}

func (s *policyMutatorStub) LocateByIdentity(context.Context, string, imapx.MessageIdentity) ([]mailmodel.MsgID, error) {
	return nil, nil
}

var testID = mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 1}

type policyFixture struct {
	Service *policy.Service
	Stub    *policyMutatorStub
}

func newPolicyFixture(t *testing.T) *policyFixture {
	t.Helper()
	t.Setenv(policy.ReadonlyEnv, "0")
	stub := &policyMutatorStub{}
	store, err := index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &policyFixture{Service: policy.New(stub, store), Stub: stub}
}

func TestMarkUnreadStoresSeenRemoval(t *testing.T) {
	f := newPolicyFixture(t)
	if err := f.Service.MarkUnread(context.Background(), testID, "message.mark-unread", ""); err != nil {
		t.Fatalf("MarkUnread: %v", err)
	}
	if got := f.Stub.LastSetFlags; got.add != nil || !slices.Equal(got.remove, []string{"\\Seen"}) {
		t.Fatalf("SetFlags args: add=%v remove=%v", got.add, got.remove)
	}
}

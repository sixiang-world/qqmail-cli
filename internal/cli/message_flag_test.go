package cli

import (
	"context"
	"slices"
	"testing"
)

var testIDString = testID.String()

func TestFlagRejectsAnythingButFlagged(t *testing.T) {
	runExit(t, 2, "message", "flag", testIDString, "--add", "\\Seen", "--json")                             // 值域白名单 → usage
	runExit(t, 2, "message", "flag", testIDString, "--json")                                                // add/remove 缺一 → usage
	runExit(t, 2, "message", "flag", testIDString, "--add", "\\Flagged", "--remove", "\\Flagged", "--json") // 互斥 → usage
}

func TestFlagMessageStoresFlagged(t *testing.T) {
	f := newPolicyFixture(t)
	if err := f.Service.FlagMessage(context.Background(), testID, true, "message.flag", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := f.Stub.LastSetFlags; !slices.Equal(got.add, []string{"\\Flagged"}) || got.remove != nil {
		t.Fatalf("add args: %+v", got)
	}
	// remove 同理断言 -FLAGS \Flagged
	if err := f.Service.FlagMessage(context.Background(), testID, false, "message.flag", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := f.Stub.LastSetFlags; got.add != nil || !slices.Equal(got.remove, []string{"\\Flagged"}) {
		t.Fatalf("remove args: add=%v remove=%v", got.add, got.remove)
	}
}

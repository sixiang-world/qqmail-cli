package imapx

import (
	"context"
	"strings"
	"testing"
)

// LastCreateLine returns the most recent client command line carrying a CREATE
// command (e.g. `A003 CREATE "arch/&Xv9kGg-"`).
func (f *writableFixture) LastCreateLine() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToUpper(f.lines[i]), "CREATE") {
			return f.lines[i]
		}
	}
	return ""
}

// LastRenameLine returns the most recent client command line carrying a RENAME
// command (e.g. `A004 RENAME "old" "new"`).
func (f *writableFixture) LastRenameLine() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToUpper(f.lines[i]), "RENAME") {
			return f.lines[i]
		}
	}
	return ""
}

// The wire expectations are the literal bytes captured from this fixture with
// go-imap v2.0.0-beta.8 serializing the raw name: RFC 3501 modified UTF-7
// (&-shift form) — 账单 = &jSZTVQ-. EncodeMailbox cannot express this form (it
// emits RFC 2152 '+'-shift), which is exactly why the client passes the raw
// name and lets go-imap encode, the same path message move uses.
func TestCreateAndRenameFolderLines(t *testing.T) {
	f := newWritableFixture(t) // Task 4 建立的记录型 fake；folder 操作不依赖邮件选中态
	if err := f.Mutator.CreateFolder(context.Background(), "2026-账单"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := f.LastCreateLine(); !strings.Contains(got, `CREATE "2026-&jSZTVQ-"`) {
		t.Fatalf("create line: %q", got)
	}
	if err := f.Mutator.RenameFolder(context.Background(), "2026-账单", "2027-账单"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := f.LastRenameLine(); !strings.Contains(got, `RENAME "2026-&jSZTVQ-" "2027-&jSZTVQ-"`) {
		t.Fatalf("rename line: %q", got)
	}
}

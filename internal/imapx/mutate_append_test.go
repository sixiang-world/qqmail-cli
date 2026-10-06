package imapx

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// appendRecording captures one APPEND command the fixture saw on the wire: the
// mailbox, the flag list, the declared literal size, and the raw literal bytes
// themselves.
type appendRecording struct {
	folder string
	flags  []string
	size   int64
	body   []byte
}

// LastAppend returns the most recent APPEND the fixture served (zero value
// when none was seen).
func (f *writableFixture) LastAppend() appendRecording {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appends) == 0 {
		return appendRecording{}
	}
	return f.appends[len(f.appends)-1]
}

func (f *writableFixture) recordAppend(info appendRecording) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, info)
}

// parseAppendLine extracts folder/flags/size from an APPEND command line such
// as `A005 APPEND "Drafts" (\Draft) {8}`. The mailbox may be a quoted string
// containing spaces ("My Drafts") — the form go-imap emits for any name with
// whitespace — so the mailbox argument is sliced from the raw line rather than
// taken from whitespace-split fields.
func parseAppendLine(line string) appendRecording {
	var info appendRecording
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.EqualFold(fields[1], "APPEND") {
		return info
	}
	afterKeyword := line[strings.Index(line, fields[1])+len(fields[1]):]
	mailbox := strings.TrimSpace(afterKeyword)
	if strings.HasPrefix(mailbox, `"`) {
		end := strings.IndexByte(mailbox[1:], '"')
		if end < 0 {
			return info
		}
		info.folder = mailbox[1 : 1+end]
		mailbox = mailbox[end+2:]
	} else {
		info.folder = fields[2]
		mailbox = strings.TrimPrefix(mailbox, info.folder)
	}
	for _, field := range strings.Fields(mailbox) {
		if strings.HasPrefix(field, "(") && strings.HasSuffix(field, ")") {
			info.flags = strings.Fields(strings.Trim(field, "()"))
		}
		if strings.HasPrefix(field, "{") && strings.HasSuffix(field, "}") {
			if size, err := strconv.ParseInt(strings.Trim(field, "{}"), 10, 64); err == nil {
				info.size = size
			}
		}
	}
	return info
}

// TestParseAppendLineMailboxForms pins the parser to the mailbox argument
// shapes the protocol allows: a quoted name with spaces, with and without a
// flag list, plus the bare-atom form.
func TestParseAppendLineMailboxForms(t *testing.T) {
	for _, tc := range []struct {
		line   string
		folder string
		flags  int
		size   int64
	}{
		{`A01 APPEND "My Drafts" (\Draft) {8}`, "My Drafts", 1, 8},
		{`A02 APPEND "My Drafts" {5}`, "My Drafts", 0, 5},
		{`A03 APPEND Drafts (\Draft) {8}`, "Drafts", 1, 8},
	} {
		got := parseAppendLine(tc.line)
		if got.folder != tc.folder || len(got.flags) != tc.flags || got.size != tc.size {
			t.Fatalf("parseAppendLine(%q) = %+v, want folder %q, %d flag(s), size %d", tc.line, got, tc.folder, tc.flags, tc.size)
		}
	}
}

func TestAppendDraftSendsDraftFlag(t *testing.T) {
	f := newWritableFixture(t) // Task 4 建立的记录型 fake，扩展记录 APPEND
	// The mailbox deliberately contains a space: it pins both the client's
	// quoted-string serialization and the fixture's parseAppendLine to the
	// `APPEND "My Drafts" (\Draft) {8}` wire shape.
	if err := f.Mutator.AppendDraft(context.Background(), "My Drafts", []byte("MIME-RAW")); err != nil {
		t.Fatalf("AppendDraft: %v", err)
	}
	if got := f.LastAppend(); got.folder != "My Drafts" || !slices.Contains(got.flags, "\\Draft") || got.size != int64(8) {
		t.Fatalf("append: %+v", got)
	}
	if string(f.LastAppend().body) != "MIME-RAW" {
		t.Fatalf("literal body: %q", f.LastAppend().body)
	}
}

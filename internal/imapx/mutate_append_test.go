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
// as `A005 APPEND "Drafts" (\Draft) {8}`.
func parseAppendLine(line string) appendRecording {
	var info appendRecording
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.EqualFold(fields[1], "APPEND") {
		return info
	}
	info.folder = strings.Trim(fields[2], `"`)
	for _, field := range fields[3:] {
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

func TestAppendDraftSendsDraftFlag(t *testing.T) {
	f := newWritableFixture(t) // Task 4 建立的记录型 fake，扩展记录 APPEND
	if err := f.Mutator.AppendDraft(context.Background(), "Drafts", []byte("MIME-RAW")); err != nil {
		t.Fatalf("AppendDraft: %v", err)
	}
	if got := f.LastAppend(); got.folder != "Drafts" || !slices.Contains(got.flags, "\\Draft") || got.size != int64(8) {
		t.Fatalf("append: %+v", got)
	}
	if string(f.LastAppend().body) != "MIME-RAW" {
		t.Fatalf("literal body: %q", f.LastAppend().body)
	}
}

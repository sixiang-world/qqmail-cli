package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
)

type searchModeReader struct {
	filters []imapx.SearchFilter
}

func (r *searchModeReader) Capabilities() ([]string, []string) { return nil, nil }
func (r *searchModeReader) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return nil, nil
}
func (r *searchModeReader) Examine(context.Context, string) (uint32, uint32, error) {
	return 1, 1, nil
}
func (r *searchModeReader) Search(_ context.Context, filter imapx.SearchFilter) ([]uint32, error) {
	r.filters = append(r.filters, filter)
	return []uint32{1}, nil
}
func (r *searchModeReader) FetchEnvelopes(context.Context, string, uint32, []uint32) ([]mailmodel.Envelope, error) {
	return []mailmodel.Envelope{{UID: 1, Subject: "中文测试"}}, nil
}
func (r *searchModeReader) FetchHeaderFields(context.Context, []uint32) ([]mailmodel.HeaderFields, error) {
	return nil, nil
}
func (r *searchModeReader) FetchMessage(context.Context, mailmodel.MsgID) ([]byte, error) {
	return nil, nil
}
func (r *searchModeReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return nil, false, nil
}
func (r *searchModeReader) Logout(context.Context) error { return nil }

func TestNonASCIISearchUsesClientWindow(t *testing.T) {
	reader := &searchModeReader{}
	items, _, mode, err := listEnvelopes(context.Background(), reader, "INBOX", imapx.SearchFilter{Subject: "中文", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "client_window" || len(items) != 1 {
		t.Fatalf("mode=%q items=%#v", mode, items)
	}
	if len(reader.filters) != 1 || reader.filters[0].Subject != "" {
		t.Fatalf("server received unsafe non-ASCII filter: %#v", reader.filters)
	}
}

// fallbackSearchReader records the criteria of every SEARCH and can refuse
// specific ones, mimicking a server that rejects composite criteria; the
// envelope fixture matches From/To substrings but sits after the test's
// --before bound so the client-side re-filters are observable.
type fallbackSearchReader struct {
	searchModeReader
	received []imapx.SearchFilter
	reject   func(imapx.SearchFilter) bool
}

func (r *fallbackSearchReader) Search(_ context.Context, filter imapx.SearchFilter) ([]uint32, error) {
	r.received = append(r.received, filter)
	if r.reject != nil && r.reject(filter) {
		return nil, errors.New("NO unsupported search criteria")
	}
	return []uint32{7}, nil
}

func (r *fallbackSearchReader) FetchEnvelopes(context.Context, string, uint32, []uint32) ([]mailmodel.Envelope, error) {
	return []mailmodel.Envelope{{
		UID:     7,
		Subject: "hello",
		From:    []mailmodel.Address{{Email: "alice@example.com"}},
		To:      []mailmodel.Address{{Email: "bob@example.com"}},
		InternalDate: time.Date(2026, 6, 1, 12, 0, 0, 0, time.Local),
	}}, nil
}

// The error-retry fallback must strip To together with From/Subject: a second
// SEARCH that still carries the refused criterion would be refused again, and
// the To substring must keep filtering client-side inside the fallback window.
func TestEnvelopeFallbackStripsToFromServerCriteria(t *testing.T) {
	reader := &fallbackSearchReader{reject: func(filter imapx.SearchFilter) bool {
		return filter.From != "" || filter.To != ""
	}}
	items, _, mode, err := listEnvelopes(context.Background(), reader, "INBOX", imapx.SearchFilter{From: "alice", To: "bob", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "client_window" {
		t.Fatalf("mode=%q, want client_window", mode)
	}
	if len(reader.received) != 2 {
		t.Fatalf("want exactly 2 SEARCHes, got %#v", reader.received)
	}
	if reader.received[0].From != "alice" || reader.received[0].To != "bob" {
		t.Fatalf("first SEARCH lost the requested criteria: %#v", reader.received[0])
	}
	if reader.received[1].From != "" || reader.received[1].To != "" || reader.received[1].Subject != "" {
		t.Fatalf("retry still carries text criteria: %#v", reader.received[1])
	}
	if len(items) != 1 || len(items[0].To) != 1 || items[0].To[0].Email != "bob@example.com" {
		t.Fatalf("client-side To filter not applied in fallback window: %#v", items)
	}
}

// --before is enforced client-side too: an envelope whose InternalDate falls
// after the bound is dropped even when the server window returned it.
func TestEnvelopeBeforeFiltersClientSide(t *testing.T) {
	reader := &fallbackSearchReader{}
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	items, _, mode, err := listEnvelopes(context.Background(), reader, "INBOX", imapx.SearchFilter{Before: before, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "server" {
		t.Fatalf("mode=%q, want server", mode)
	}
	if len(reader.received) != 1 || reader.received[0].Before != before {
		t.Fatalf("server did not receive the Before bound: %#v", reader.received)
	}
	if len(items) != 0 {
		t.Fatalf("envelope after --before was not dropped client-side: %#v", items)
	}
}

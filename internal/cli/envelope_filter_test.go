package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/secrets"
)

// envelopeFilterReader records every SearchFilter the command layer builds, so
// tests can assert which criteria actually reached the server.
type envelopeFilterReader struct {
	filters []imapx.SearchFilter
}

func (r *envelopeFilterReader) Capabilities() ([]string, []string) { return nil, nil }
func (r *envelopeFilterReader) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return nil, nil
}
func (r *envelopeFilterReader) Examine(context.Context, string) (uint32, uint32, error) {
	return 1, 1, nil
}
func (r *envelopeFilterReader) Search(_ context.Context, filter imapx.SearchFilter) ([]uint32, error) {
	r.filters = append(r.filters, filter)
	return []uint32{1}, nil
}
func (r *envelopeFilterReader) FetchEnvelopes(context.Context, string, uint32, []uint32) ([]mailmodel.Envelope, error) {
	return []mailmodel.Envelope{{UID: 1, Subject: "fixture", To: []mailmodel.Address{{Email: "alice@example.com"}}}}, nil
}
func (r *envelopeFilterReader) FetchHeaderFields(context.Context, []uint32) ([]mailmodel.HeaderFields, error) {
	return nil, nil
}
func (r *envelopeFilterReader) FetchMessage(context.Context, mailmodel.MsgID) ([]byte, error) {
	return nil, nil
}
func (r *envelopeFilterReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return nil, false, nil
}
func (r *envelopeFilterReader) Logout(context.Context) error { return nil }

func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	return runCLIReader(t, fakeReader{}, args...)
}

// runCLIReader runs one command against the given reader and returns stdout,
// failing the test on any error.
func runCLIReader(t *testing.T, reader imapx.Reader, args ...string) string {
	t.Helper()
	out, err := tryRunCLI(t, reader, args)
	if err != nil {
		t.Fatalf("command %v failed: %v", args, err)
	}
	return out
}

// runExit runs one command and asserts its mapped process exit code
// (errmap.Details), failing the test when the command succeeds.
func runExit(t *testing.T, wantCode int, args ...string) {
	t.Helper()
	_, err := tryRunCLI(t, fakeReader{}, args)
	if err == nil {
		t.Fatalf("command %v unexpectedly succeeded", args)
	}
	_, code := errmap.Details(err)
	if code != wantCode {
		t.Fatalf("exit code %d, want %d: %v", code, wantCode, err)
	}
}

func tryRunCLI(t *testing.T, reader imapx.Reader, args []string) (string, error) {
	t.Helper()
	configPath := saveSendConfig(t, nil)
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(""),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return reader, nil },
	}
	root := NewRoot(rt)
	root.SetArgs(append([]string{"--config", configPath}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestEnvelopeListBeforeAcceptsSameWindowsAsSince(t *testing.T) {
	runCLI(t, "envelope", "list", "--before", "7d", "--json")         // 相对窗口，复用 parseSince
	runCLI(t, "envelope", "list", "--before", "2026-02-01", "--json") // 绝对日期
	runExit(t, 2, "envelope", "list", "--before", "not-a-date", "--json")

	reader := &envelopeFilterReader{}
	absolute := runCLIReader(t, reader, "envelope", "list", "--before", "2026-02-01", "--json")
	if !strings.Contains(absolute, `"filters_applied":["before"]`) {
		t.Fatalf("meta.filters_applied missing \"before\": %s", absolute)
	}
	if len(reader.filters) != 1 || reader.filters[0].Before.Month() != time.February || reader.filters[0].Before.Day() != 1 || reader.filters[0].Before.Year() != 2026 {
		t.Fatalf("absolute --before window not parsed like --since: %#v", reader.filters)
	}
}

func TestEnvelopeListToPassesThrough(t *testing.T) {
	reader := &envelopeFilterReader{}
	out := runCLIReader(t, reader, "envelope", "list", "--to", "alice@example.com", "--json")
	if !strings.Contains(out, `"filters_applied":["to"]`) {
		t.Fatalf("meta.filters_applied missing \"to\": %s", out)
	}
	if len(reader.filters) != 1 || reader.filters[0].To != "alice@example.com" {
		t.Fatalf("server did not receive TO criteria: %#v", reader.filters)
	}
}

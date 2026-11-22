package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/secrets"
)

type resumableWatchReader struct {
	watchReader
	searches []imapx.SearchFilter
}

func (r *resumableWatchReader) Search(_ context.Context, filter imapx.SearchFilter) ([]uint32, error) {
	r.searches = append(r.searches, filter)
	return []uint32{6}, nil
}

func TestWatchExplicitResumeCheckpoint(t *testing.T) {
	reader := &resumableWatchReader{}
	out := runCLIReader(t, reader, "watch", "--once", "--jsonl", "--resume-uid", "5", "--resume-uidvalidity", "10")
	if len(reader.searches) != 1 || reader.searches[0].AfterUID != 5 {
		t.Fatalf("resume cursor was not used: %+v", reader.searches)
	}
	var event struct {
		Event    string `json:"event"`
		Envelope struct {
			UID uint32 `json:"uid"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(out), &event); err != nil {
		t.Fatal(err)
	}
	if event.Event != "new_message" || event.Envelope.UID != 6 {
		t.Fatalf("wrong event: %+v", event)
	}
}

func TestWatchResumeRequiresBothCoordinates(t *testing.T) {
	runExit(t, 2, "watch", "--jsonl", "--once", "--resume-uid", "5")
	runExit(t, 2, "watch", "--jsonl", "--once", "--resume-uidvalidity", "10")
}

type changingInboxReader struct {
	watchReader
	ids      []uint32
	searches []imapx.SearchFilter
}

func (r *changingInboxReader) Search(_ context.Context, f imapx.SearchFilter) ([]uint32, error) {
	r.searches = append(r.searches, f)
	out := []uint32{}
	for _, uid := range r.ids {
		if uid > f.AfterUID && (f.BeforeUID == 0 || uid < f.BeforeUID) {
			out = append(out, uid)
			if f.Limit > 0 && len(out) == f.Limit {
				break
			}
		}
	}
	return out, nil
}

func TestWatchOnceBootstrapsResumableCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial []uint32 // descending, as returned by IMAP search
		lastUID uint32
		nextUID uint32
	}{
		{name: "empty mailbox", nextUID: 1},
		{name: "existing mail", initial: []uint32{20, 15}, lastUID: 20, nextUID: 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &changingInboxReader{ids: tc.initial}
			configPath := saveSendConfig(t, nil)
			runOnce := func(args ...string) string {
				t.Helper()
				var out bytes.Buffer
				rt := &Runtime{
					Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""),
					Secrets:      &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
					Dial:         func(context.Context, account.Named, string) (imapx.Reader, error) { return reader, nil },
					IndexInspect: func(string) (index.Inspection, error) { return index.Inspection{}, nil },
				}
				root := NewRoot(rt)
				root.SetArgs(append([]string{"--config", configPath, "watch", "--once", "--jsonl"}, args...))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				return out.String()
			}
			out := runOnce()
			var checkpoint watchEvent
			if err := json.Unmarshal([]byte(out), &checkpoint); err != nil {
				t.Fatalf("expected one bootstrap checkpoint, got %q: %v", out, err)
			}
			if checkpoint.Event != "checkpoint" || checkpoint.LastUID == nil || *checkpoint.LastUID != tc.lastUID ||
				checkpoint.UIDValidity != 10 || checkpoint.Folder != "INBOX" || checkpoint.Envelope != nil {
				t.Fatalf("wrong bootstrap checkpoint: %s", out)
			}
			validateOutput(t, "watch.event.schema.json", []byte(out))

			reader.ids = append([]uint32{tc.nextUID}, reader.ids...)
			out = runOnce("--resume-uid", strconv.FormatUint(uint64(*checkpoint.LastUID), 10),
				"--resume-uidvalidity", strconv.FormatUint(uint64(checkpoint.UIDValidity), 10))
			var event watchEvent
			if err := json.Unmarshal([]byte(out), &event); err != nil {
				t.Fatalf("expected exactly one new message event, got %q: %v", out, err)
			}
			if event.Event != "new_message" || event.Envelope == nil || event.Envelope.UID != tc.nextUID {
				t.Fatalf("new mail missed after resuming from checkpoint: %s", out)
			}
			if reader.searches[len(reader.searches)-1].Limit != 0 {
				t.Fatalf("explicit checkpoint bootstrapped instead of reading: %+v", reader.searches)
			}
		})
	}
}

func TestWatchResumeExplicitZeroCannotBeOverriddenByIndex(t *testing.T) {
	reader := &changingInboxReader{ids: []uint32{1}}
	// A valid explicit checkpoint must not probe the stale local index at all.
	configPath := saveSendConfig(t, nil)
	var out bytes.Buffer
	rt := &Runtime{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return reader, nil },
		IndexInspect: func(string) (index.Inspection, error) {
			t.Fatal("index must not override explicit checkpoint")
			return index.Inspection{}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "watch", "--once", "--jsonl", "--resume-uid", "0", "--resume-uidvalidity", "10"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"new_message"`) {
		t.Fatalf("missing message: %s", out.String())
	}
}

func TestWatchResumeInvalidCoordinatePairs(t *testing.T) {
	for _, args := range [][]string{
		{"--resume-uid", "0"},
		{"--resume-uidvalidity", "10"},
		{"--resume-uid", "1", "--resume-uidvalidity", "0"},
		{"--resume-uid", "0", "--resume-uidvalidity", "0"},
	} {
		runExit(t, 2, append([]string{"watch", "--jsonl", "--once"}, args...)...)
	}
}

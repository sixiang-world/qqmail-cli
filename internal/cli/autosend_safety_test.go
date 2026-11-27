package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

func TestAutoSendCorruptStateFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	named := account.Named{AutoSend: true, DailyAutoLimit: 3}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"malformed JSON", "{not valid json"},
		{"missing count", `{"date":"2026-11-23"}`},
		{"null count", `{"date":"2026-11-23","count":null}`},
		{"missing count on previous day", `{"date":"2026-11-22"}`},
		{"null count on previous day", `{"date":"2026-11-22","count":null}`},
		{"missing date", `{"count":1}`},
		{"null date", `{"date":null,"count":1}`},
		{"empty object", `{}`},
		{"null state", `null`},
		{"negative count", `{"date":"2026-11-23","count":-1}`},
		{"string count", `{"date":"2026-11-23","count":"1"}`},
		{"fractional count", `{"date":"2026-11-23","count":1.5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := withAutoSendStateDir(t)
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			eligible, err := autoSendEligible(named, []string{"allowed@example.com"}, now)
			if eligible || err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
				t.Fatalf("corrupt state must deny autosend: eligible=%v err=%v", eligible, err)
			}
			err = reserveAutoSend(named, now)
			if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
				t.Fatalf("reservation must fail closed on corrupt state: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != tc.raw {
				t.Fatalf("corrupt counter overwritten: %q %v", got, err)
			}
		})
	}
}

func TestAutoSendExplicitZeroCountCanReserve(t *testing.T) {
	withAutoSendStateDir(t)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	seedAutoSendState(t, now.Format("2006-01-02"), 0)
	named := account.Named{AutoSend: true, DailyAutoLimit: 3}
	eligible, err := autoSendEligible(named, []string{"allowed@example.com"}, now)
	if !eligible || err != nil {
		t.Fatalf("explicit zero count must allow autosend: eligible=%v err=%v", eligible, err)
	}
	if err := reserveAutoSend(named, now); err != nil {
		t.Fatal(err)
	}
	state, err := readAutoSendState(now)
	if err != nil || state.Date != now.Format("2006-01-02") || state.Count != 1 {
		t.Fatalf("zero-count reservation=%+v err=%v", state, err)
	}
}

func TestAutoSendReservationsAreAtomic(t *testing.T) {
	withAutoSendStateDir(t)
	const requests, limit = 32, 7
	named := account.Named{AutoSend: true, DailyAutoLimit: limit}
	now := time.Now()
	var wg sync.WaitGroup
	outcomes := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes <- reserveAutoSend(named, now)
		}()
	}
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else if errmap.Classify(err).Kind != errmap.PolicyDenied {
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if successes != limit {
		t.Fatalf("reserved %d, want %d", successes, limit)
	}
	state, err := readAutoSendState(now)
	if err != nil || state.Count != limit {
		t.Fatalf("counter=%+v err=%v", state, err)
	}
}

func TestAutoSendFutureCounterDateFailsClosed(t *testing.T) {
	path := withAutoSendStateDir(t)
	now := time.Date(2026, 11, 6, 23, 59, 0, 0, time.Local)
	raw := []byte(`{"date":"2026-11-07","count":1}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	named := account.Named{AutoSend: true, DailyAutoLimit: 3}
	if err := reserveAutoSend(named, now); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("clock rollback must deny unattended send: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatalf("future-dated counter overwritten: %q %v", got, err)
	}
}

func TestAutoSendSharedGlobalCounterAcrossAccounts(t *testing.T) {
	withAutoSendStateDir(t)
	now := time.Now()
	first := account.Named{AutoSend: true, DailyAutoLimit: 2, Email: "one@qq.com"}
	second := account.Named{AutoSend: true, DailyAutoLimit: 2, Email: "two@qq.com"}
	if err := reserveAutoSend(first, now); err != nil {
		t.Fatal(err)
	}
	if err := reserveAutoSend(second, now); err != nil {
		t.Fatal(err)
	}
	if err := reserveAutoSend(first, now); err == nil {
		t.Fatal("shared global daily cap bypassed using another account")
	}
}

func TestAutoSendReservationSubprocessHelper(t *testing.T) {
	path := os.Getenv("QQMAIL_TEST_AUTOSEND_PATH")
	if path == "" {
		return
	}
	autoSendStatePath = func() (string, error) { return path, nil }
	named := account.Named{AutoSend: true, DailyAutoLimit: 4}
	err := reserveAutoSend(named, time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local))
	if err == nil {
		return
	}
	if errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("reservation error: %v", err)
	}
	// The parent recognizes the intentional cap denial as a successful helper exit.
}

func TestAutoSendQuotaIndependentProcesses(t *testing.T) {
	if os.Getenv("QQMAIL_TEST_AUTOSEND_PATH") != "" {
		t.Skip("helper process")
	}
	path := filepath.Join(t.TempDir(), "autosend-state.json")
	const count = 12
	type result struct {
		stdout string
		err    error
	}
	outcomes := make(chan result, count)
	for i := 0; i < count; i++ {
		go func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAutoSendReservationSubprocessHelper$")
			cmd.Env = append(os.Environ(), "QQMAIL_TEST_AUTOSEND_PATH="+path)
			out, err := cmd.CombinedOutput()
			outcomes <- result{stdout: string(out), err: err}
		}()
	}
	for i := 0; i < count; i++ {
		r := <-outcomes
		if r.err != nil {
			t.Fatalf("subprocess reservation failed: %v %s", r.err, r.stdout)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state autoSendState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Count != 4 || state.Date != "2026-11-06" {
		t.Fatalf("cross-process cap bypassed: %+v / %s", state, strings.TrimSpace(string(raw)))
	}
}

// A misspelled key such as send_blaklist must never silently disable a safety
// rule while auto_send = true remains active: autosend fails closed with
// PolicyDenied, while manual TTY sends stay backward-compatible.
func TestAutoSendUnknownKeysFailClosed(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")

	cases := []struct {
		name      string
		configRaw string
		wantKey   string
	}{
		{
			name: "misspelled account send_blacklist fails closed",
			configRaw: `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = true
send_blaklist = ["reader@example.com"]
`,
			wantKey: "accounts.personal.send_blaklist",
		},
		{
			name: "misplaced top-level send_blacklist fails closed",
			configRaw: `
schema = 1
default_account = "personal"
send_blacklist = ["reader@example.com"]

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = true
`,
			wantKey: "send_blacklist",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(configPath, []byte(tc.configRaw), 0o600); err != nil {
				t.Fatal(err)
			}
			sent := false
			rt := &Runtime{
				Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
				Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
				IsTerminal: func(io.Reader) bool { return false },
				SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
					sent = true
					return nil
				},
			}
			root := NewRoot(rt)
			root.SetArgs([]string{"--config", configPath, "send", "--draft-file", letter, "--execute"})
			err := root.Execute()
			if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied || sent {
				t.Fatalf("unknown key must fail closed on autosend: err=%v sent=%v", err, sent)
			}
			keys, _ := errmap.Classify(err).Context["unknown_keys"].([]string)
			if len(keys) == 0 || keys[0] != tc.wantKey {
				t.Fatalf("expected unknown_keys=%q in context, got %#v", tc.wantKey, errmap.Classify(err).Context)
			}
		})
	}

	t.Run("reserveAutoSend refuses unknown keys as defense in depth", func(t *testing.T) {
		named := account.Named{AutoSend: true, DailyAutoLimit: 50, UnknownKeys: []string{"accounts.personal.send_blaklist"}}
		err := reserveAutoSend(named, time.Now())
		if err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
			t.Fatalf("reserveAutoSend must fail closed on unknown keys: %v", err)
		}
	})

	t.Run("manual send with auto_send=false remains backward-compatible", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.toml")
		raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = false
extra_note = "legacy"
`
		if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cachePath := testCachePath(t)
		sent := 0
		rt := &Runtime{
			Build: BuildInfo{Version: "test"}, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader("SEND\n"),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return true },
			IndexOpen: func(_ string, write bool) (*index.DB, error) {
				return index.OpenPath(cachePath, write)
			},
			SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
				sent++
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--draft-file", letter, "--execute"})
		if err := root.Execute(); err != nil {
			t.Fatalf("manual TTY send should stay lenient when auto_send=false: %v", err)
		}
		if sent != 1 {
			t.Fatalf("sent = %d, want 1", sent)
		}
	})
}

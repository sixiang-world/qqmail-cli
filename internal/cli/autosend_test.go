package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

// withAutoSendStateDir redirects the local daily counter into a temp dir and
// returns the state file path so tests can seed or inspect it.
func withAutoSendStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "autosend-state.json")
	previous := autoSendStatePath
	autoSendStatePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { autoSendStatePath = previous })
	return path
}

func seedAutoSendState(t *testing.T, date string, count int) {
	t.Helper()
	path, err := autoSendStatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(autoSendState{Date: date, Count: count})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAutoSendCountForTest(t *testing.T) int {
	t.Helper()
	path, err := autoSendStatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	var state autoSendState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("state file corrupt: %v", err)
	}
	return state.Count
}

func saveAutoSendConfig(t *testing.T, allowlist []string, autoSend bool, blacklist []string, dailyLimit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &account.Config{Schema: account.ConfigSchema, DefaultAccount: "personal", Accounts: map[string]account.Account{
		"personal": {Email: "user@qq.com", SendAllowlist: allowlist, AutoSend: autoSend, SendBlacklist: blacklist, DailyAutoLimit: dailyLimit},
	}}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// autoSendEligible is the pure decision core: auto_send off → interactive;
// any blacklisted recipient → interactive; the daily cap → policy_denied;
// otherwise (with a stale-dated counter resetting to zero) → auto.
func TestAutoSendEligible(t *testing.T) {
	withAutoSendStateDir(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	t.Run("auto_send off stays interactive", func(t *testing.T) {
		named := account.Named{AutoSend: false, DailyAutoLimit: 50}
		auto, err := autoSendEligible(named, []string{"reader@example.com"}, now)
		if auto || err != nil {
			t.Fatalf("auto_send=false must be (false,nil), got (%v,%v)", auto, err)
		}
	})

	t.Run("allowlisted recipients with no blacklist hit go auto", func(t *testing.T) {
		named := account.Named{AutoSend: true, SendBlacklist: []string{"boss@example.com"}, DailyAutoLimit: 50}
		auto, err := autoSendEligible(named, []string{"Reader@Example.com ", "cc@example.com"}, now)
		if !auto || err != nil {
			t.Fatalf("want (true,nil), got (%v,%v)", auto, err)
		}
	})

	t.Run("any blacklisted recipient falls back to TTY", func(t *testing.T) {
		named := account.Named{AutoSend: true, SendBlacklist: []string{"Trap@Example.com"}, DailyAutoLimit: 50}
		auto, err := autoSendEligible(named, []string{"reader@example.com", "trap@example.com"}, now)
		if auto || err != nil {
			t.Fatalf("blacklist hit must fall back to TTY (false,nil), got (%v,%v)", auto, err)
		}
	})

	t.Run("counter at the daily limit is policy denied", func(t *testing.T) {
		named := account.Named{AutoSend: true, DailyAutoLimit: 2}
		seedAutoSendState(t, today, 2)
		auto, err := autoSendEligible(named, []string{"reader@example.com"}, now)
		if auto || err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
			t.Fatalf("cap reached must be (false, policy_denied), got (%v,%v)", auto, err)
		}
		if !strings.Contains(err.Error(), "已达今日自动发送上限") {
			t.Fatalf("cap error must name the limit: %v", err)
		}
		_, code := errmap.Details(err)
		failure, _ := errmap.Details(err)
		if code != 50 || failure.Retryable {
			t.Fatalf("cap must be exit 50 non-retryable, got code=%d retryable=%v", code, failure.Retryable)
		}
	})

	t.Run("yesterday's counter resets to zero", func(t *testing.T) {
		named := account.Named{AutoSend: true, DailyAutoLimit: 2}
		seedAutoSendState(t, yesterday, 2)
		auto, err := autoSendEligible(named, []string{"reader@example.com"}, now)
		if !auto || err != nil {
			t.Fatalf("cross-day reset must go auto, got (%v,%v)", auto, err)
		}
	})

	t.Run("non-positive limit is treated as the 50 default", func(t *testing.T) {
		named := account.Named{AutoSend: true, DailyAutoLimit: 0}
		seedAutoSendState(t, today, 49)
		auto, err := autoSendEligible(named, []string{"reader@example.com"}, now)
		if !auto || err != nil {
			t.Fatalf("49 under the implicit default of 50 must go auto, got (%v,%v)", auto, err)
		}
		seedAutoSendState(t, today, 50)
		auto, err = autoSendEligible(named, []string{"reader@example.com"}, now)
		if auto || err == nil {
			t.Fatalf("50 at the implicit default of 50 must be denied, got (%v,%v)", auto, err)
		}
	})
}

// Invariant: with auto_send on and every recipient allowlisted, --execute
// completes without a TTY (stdin is never read) and the local counter bumps.
func TestAutoSendSendsWithoutTTY(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, true, nil, 50)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	cachePath := filepath.Join(t.TempDir(), "cache.db")
	var out, stderr bytes.Buffer
	sent := 0
	rt := &Runtime{
		Build: BuildInfo{Version: "test"}, Out: &out, Err: &stderr, In: strings.NewReader(""),
		Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		IsTerminal: func(io.Reader) bool { return false },
		IndexOpen: func(_ string, write bool) (*index.DB, error) {
			return index.OpenPath(cachePath, write)
		},
		SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
			sent++
			return nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "send", "--draft-file", letter, "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("autosend transport calls = %d, want 1", sent)
	}
	if readAutoSendCountForTest(t) != 1 {
		t.Fatal("autosend counter was not bumped")
	}
	validateOutput(t, "send.schema.json", out.Bytes())
}

// Invariant 3: a blacklisted recipient falls back to the interactive gate —
// under a non-TTY stdin that gate refuses with the TTY hint, never a send.
func TestAutoSendBlacklistFallsBackToTTY(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, true, []string{"reader@example.com"}, 50)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
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
		t.Fatalf("blacklisted autosend must fall back to the TTY gate and be refused, got %v sent=%v", err, sent)
	}
	if !strings.Contains(err.Error(), "stdin 不是 TTY") {
		t.Fatalf("fallback must hit the TTY gate itself, got %v", err)
	}
}

// Invariant 1: autosend never relaxes the allowlist — a miss stays
// policy_denied with the same denied_recipients context.
func TestAutoSendAllowlistMissStillDenied(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, true, nil, 50)
	letter := writeDraftFile(t, "to = [\"stranger@other.example\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
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
		t.Fatalf("allowlist miss must stay policy_denied, got %v sent=%v", err, sent)
	}
	denied, _ := errmap.Classify(err).Context["denied_recipients"].([]string)
	if len(denied) != 1 || denied[0] != "stranger@other.example" {
		t.Fatalf("denial must carry the denied recipient in context: %#v", errmap.Classify(err).Context)
	}
}

// Invariant 2: readonly always wins — with QQMAIL_CLI_READONLY=1 an autosend
// --execute is refused before any transport, exactly like a manual send.
func TestReadonlyBeatsAutosend(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "1")
	withAutoSendStateDir(t)
	configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, true, nil, 50)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
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
		t.Fatalf("readonly must refuse autosend execute, got %v sent=%v", err, sent)
	}
	if !strings.Contains(err.Error(), "QQMAIL_CLI_READONLY") {
		t.Fatalf("refusal must come from the readonly gate: %v", err)
	}
}

// Invariant 4: when today's counter reaches daily_auto_limit the send is
// policy_denied ("已达今日自动发送上限") — not a TTY fallback (non-TTY stdin
// would surface a different error if the gate fell back), and nothing sends.
func TestAutoSendDailyLimitDeniesAtCap(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	seedAutoSendState(t, time.Now().Format("2006-01-02"), 3)
	configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, true, nil, 3)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	sent := false
	rt := &Runtime{
		Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""),
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
		t.Fatalf("cap reached must be policy_denied before transport, got %v sent=%v", err, sent)
	}
	if !strings.Contains(err.Error(), "已达今日自动发送上限") {
		t.Fatalf("cap denial must name the daily limit: %v", err)
	}
}

// Invariant 5: an autosend result is field-for-field identical to a manual
// send's result — no "auto" marker, no new field anywhere in data.
func TestAutoSendOutputUnchanged(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	runSend := func(t *testing.T, autoSend bool, terminal bool, in string) []byte {
		t.Helper()
		configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, autoSend, nil, 50)
		cachePath := filepath.Join(t.TempDir(), "cache.db")
		var out bytes.Buffer
		rt := &Runtime{
			Build: BuildInfo{Version: "test"}, Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(in),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return terminal },
			IndexOpen: func(_ string, write bool) (*index.DB, error) {
				return index.OpenPath(cachePath, write)
			},
			SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "send", "--draft-file", letter, "--execute"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	extractData := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, raw)
		}
		return envelope.Data
	}
	manual := extractData(t, runSend(t, false, true, "SEND\n"))
	auto := extractData(t, runSend(t, true, false, ""))
	if !reflect.DeepEqual(manual, auto) {
		t.Fatalf("autosend output diverges from the manual gate:\nmanual: %#v\nauto:   %#v", manual, auto)
	}
	if len(manual) == 0 {
		t.Fatal("no data fields compared")
	}
}

// The counter is per local day: two autosends bump it to 2, and a manual
// (non-auto) send inside the same day leaves the counter untouched.
func TestAutoSendCounterScope(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	withAutoSendStateDir(t)
	letter := writeDraftFile(t, "to = [\"reader@example.com\"]\nsubject = \"信件\"\nbody = \"你好\"\n")
	runSend := func(t *testing.T, autoSend bool, terminal bool, in string) error {
		t.Helper()
		configPath := saveAutoSendConfig(t, []string{"reader@example.com"}, autoSend, nil, 50)
		cachePath := filepath.Join(t.TempDir(), "cache.db")
		rt := &Runtime{
			Build: BuildInfo{Version: "test"}, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(in),
			Secrets:    &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
			IsTerminal: func(io.Reader) bool { return terminal },
			IndexOpen: func(_ string, write bool) (*index.DB, error) {
				return index.OpenPath(cachePath, write)
			},
			SendMail: func(context.Context, account.Named, string, sendmail.Draft, []byte) error {
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "send", "--draft-file", letter, "--execute"})
		return root.Execute()
	}
	if err := runSend(t, true, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := runSend(t, true, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := readAutoSendCountForTest(t); got != 2 {
		t.Fatalf("two autosends must bump the counter to 2, got %d", got)
	}
	// A manual TTY send (autosend off, e.g. a blacklisted fallback) never
	// counts against the auto quota.
	if err := runSend(t, false, true, fmt.Sprintf("%s\n", "SEND")); err != nil {
		t.Fatal(err)
	}
	if got := readAutoSendCountForTest(t); got != 2 {
		t.Fatalf("manual send must not touch the auto counter, got %d", got)
	}
}

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/safeio"
)

// autoSendStateFile is the local daily autosend counter. It lives next to
// config.toml, is content-free (a date and a count — no recipients, no
// subjects), is never written to the audit trail and never surfaces in
// output: the sent mail and the command result stay identical to a manual
// send's. The file is global — one counter shared by every configured
// account, not per-account.
const autoSendStateFile = "autosend-state.json"

// autoSendStatePath returns the counter's path under the config directory
// (the same os.UserConfigDir pattern as account.DefaultPath). Tests override
// the var to redirect the counter into a temp dir.
var autoSendStatePath = func() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "qqmail-cli", autoSendStateFile), nil
}

type autoSendState struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// readAutoSendState loads today's counter; any failure (missing file, corrupt
// JSON, cross-day date) reads as zero so the gate stays usable and errs on
// the lenient side of a local hint — never a network fact.
func readAutoSendState(now time.Time) autoSendState {
	today := now.Format("2006-01-02")
	state := autoSendState{Date: today}
	path, err := autoSendStatePath()
	if err != nil {
		return state
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Date != today {
		return autoSendState{Date: today}
	}
	return state
}

// blackSet normalizes the blacklist into a lowercase trimmed set for the
// recipient scan.
func blackSet(entries []string) map[string]bool {
	set := make(map[string]bool, len(entries))
	for _, entry := range entries {
		set[strings.ToLower(strings.TrimSpace(entry))] = true
	}
	return set
}

// autoSendEligible decides whether this send may complete without the TTY
// confirmation: only when auto_send is on, every recipient is outside the
// blacklist, and the daily counter is under the limit. Blacklisted
// recipients fall back to the interactive gate; allowlist misses and
// readonly are refused upstream, so this function never weakens them.
func autoSendEligible(named account.Named, recipients []string, now time.Time) (bool, error) {
	if !named.AutoSend {
		return false, nil
	}
	blacklist := blackSet(named.SendBlacklist)
	for _, recipient := range recipients {
		if blacklist[strings.ToLower(strings.TrimSpace(recipient))] {
			return false, nil
		}
	}
	limit := named.DailyAutoLimit
	if limit <= 0 {
		limit = account.DefaultDailyAutoLimit
	}
	if readAutoSendState(now).Count >= limit {
		// Honest wording only: with auto_send on this gate fires before any
		// TTY branch, so retyping the command in a real terminal does NOT
		// bypass the cap — the human remedy is editing the config (or
		// waiting for the day to roll over). There is no bypass flag.
		return false, &errmap.Error{Kind: errmap.PolicyDenied, Message: fmt.Sprintf("已达今日自动发送上限 %d 封", limit), Suggestion: "明日自动重置，或由人工调整 auto_send/daily_auto_limit 配置后再执行"}
	}
	return true, nil
}

// bumpAutoSendCount increments today's local autosend counter (read-modify-
// write through safeio's atomic replace). Failures are returned to the caller
// for a stderr hint — the send itself already succeeded. The read-modify-write
// is intentionally unsynchronized: the CLI is a single-process, one-send-per-
// invocation tool, so concurrent bumps are out of scope by design; a lost
// update in a race would only under-count (TOCTOU accepted).
func bumpAutoSendCount(now time.Time) error {
	state := autoSendState{Date: now.Format("2006-01-02"), Count: 1}
	if existing := readAutoSendState(now); existing.Date == state.Date {
		state.Count = existing.Count + 1
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path, err := autoSendStatePath()
	if err != nil {
		return err
	}
	return safeio.WriteFileAtomic(path, raw, 0o600)
}

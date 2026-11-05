package cli

import (
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/situker/qqmail-cli/internal/account"
    "github.com/situker/qqmail-cli/internal/errmap"
    "github.com/situker/qqmail-cli/internal/safeio"
)

// Reservations are charged BEFORE SMTP submission and never refunded
// automatically: after a network failure delivery may be uncertain.
// This deliberately preserves the historical global daily cap.
const autoSendStateFile = "autosend-state.json"

var autoSendStatePath = func() (string, error) {
    base, err := os.UserConfigDir()
    if err != nil { return "", err }
    return filepath.Join(base, "qqmail-cli", autoSendStateFile), nil
}

type autoSendState struct {
    Date string `json:"date"`
    Count int `json:"count"`
}

func readAutoSendState(now time.Time) (autoSendState, error) {
    fresh := autoSendState{Date: now.Format("2006-01-02")}
    path, err := autoSendStatePath()
    if err != nil { return fresh, err }
    raw, err := os.ReadFile(path)
    if errors.Is(err, os.ErrNotExist) { return fresh, nil }
    if err != nil { return fresh, err }
    var state autoSendState
    if err := json.Unmarshal(raw, &state); err != nil {
        return fresh, fmt.Errorf("autosend counter is corrupt: %w", err)
    }
    if state.Count < 0 || state.Date == "" {
        return fresh, fmt.Errorf("autosend counter has invalid fields")
    }
    if state.Date != fresh.Date { return fresh, nil }
    return state, nil
}

func blackSet(entries []string) map[string]bool {
    set := make(map[string]bool, len(entries))
    for _, entry := range entries { set[strings.ToLower(strings.TrimSpace(entry))] = true }
    return set
}

func autoSendCapError(limit int) error {
    return &errmap.Error{
        Kind: errmap.PolicyDenied,
        Message: fmt.Sprintf("已达今日自动发送上限 %d 封", limit),
        Suggestion: "明日自动重置，或由人工调整 auto_send/daily_auto_limit 配置后再执行",
    }
}

func autoSendStateError(err error) error {
    return &errmap.Error{
        Kind: errmap.PolicyDenied,
        Message: "自动发送计数不可验证，已拒绝发送（fail closed）",
        Suggestion: "请人工检查 autosend-state.json 与文件权限，勿删除计数以绕过额度",
        Cause: err,
    }
}

func effectiveAutoSendLimit(named account.Named) int {
    if named.DailyAutoLimit > 0 { return named.DailyAutoLimit }
    return account.DefaultDailyAutoLimit
}

// This preflight is intentionally advisory; the locked reservation below is
// authoritative. It remains here for fast error reporting to callers.
func autoSendEligible(named account.Named, recipients []string, now time.Time) (bool, error) {
    if !named.AutoSend { return false, nil }
    blacklist := blackSet(named.SendBlacklist)
    for _, recipient := range recipients {
        if blacklist[strings.ToLower(strings.TrimSpace(recipient))] { return false, nil }
    }
    state, err := readAutoSendState(now)
    if err != nil { return false, autoSendStateError(err) }
    if state.Count >= effectiveAutoSendLimit(named) {
        return false, autoSendCapError(effectiveAutoSendLimit(named))
    }
    return true, nil
}

// reserveAutoSend is the only permission to proceed with an unattended send.
// A process-wide OS file lock protects the read/modify/atomic-write sequence
// across separate CLI processes; a failure consumes no additional quota.
// A successful reservation counts even if SMTP subsequently fails, since a
// failed SMTP call can leave delivery uncertain.
func reserveAutoSend(named account.Named, now time.Time) error {
    path, err := autoSendStatePath()
    if err != nil { return autoSendStateError(err) }
    unlock, err := lockAutoSend(path + ".lock")
    if err != nil { return autoSendStateError(err) }
    defer func() { _ = unlock() }()
    state, err := readAutoSendState(now)
    if err != nil { return autoSendStateError(err) }
    if state.Count >= effectiveAutoSendLimit(named) {
        return autoSendCapError(effectiveAutoSendLimit(named))
    }
    state.Count++
    raw, err := json.Marshal(state)
    if err != nil { return autoSendStateError(err) }
    if err := safeio.WriteFileAtomic(path, raw, 0o600); err != nil {
        return autoSendStateError(err)
    }
    return nil
}

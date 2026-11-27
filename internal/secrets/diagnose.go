package secrets

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/output"
	keyring "github.com/zalando/go-keyring"
)

// Reason is a stable machine-readable category for credential store failures.
type Reason string

const (
	ReasonNoSessionBus       Reason = "no_session_bus"
	ReasonServiceUnavailable Reason = "service_unavailable"
	ReasonLocked             Reason = "locked"
	ReasonPermissionDenied   Reason = "permission_denied"
	ReasonTimeout            Reason = "timeout"
	ReasonUnknown            Reason = "unknown"
)

const maxObservedRunes = 240

var (
	errKeyringTimeout = errors.New("credential store operation timed out")

	// Package-level seams for unit tests (never hit the real OS keyring or host env).
	currentGOOS = func() string { return runtime.GOOS }
	lookupEnv   = os.LookupEnv
	statPath    = os.Stat
	currentUID  = os.Getuid
)

// EnvironmentCheck summarizes non-invasive credential store readiness hints.
// It inspects platform and local environment/socket state without touching
// credentials, running external binaries, or contacting the network.
type EnvironmentCheck struct {
	Platform          string `json:"platform"`
	CredentialStore   string `json:"credential_store"`
	Ready             bool   `json:"ready"`
	Reason            Reason `json:"reason,omitempty"`
	Detail            string `json:"detail,omitempty"`
	Suggestion        string `json:"suggestion,omitempty"`
	DBusSessionBusSet *bool  `json:"dbus_session_bus_set,omitempty"`
	DBusSocketExists  *bool  `json:"dbus_socket_exists,omitempty"`
}

// Diagnosis describes a classified credential store failure and actionable
// recovery guidance for the current platform.
type Diagnosis struct {
	CredentialStore   string `json:"credential_store"`
	Reason            Reason `json:"reason"`
	Platform          string `json:"platform"`
	Observed          string `json:"observed,omitempty"`
	Suggestion        string `json:"suggestion"`
	DBusSessionBusSet *bool  `json:"dbus_session_bus_set,omitempty"`
}

// Context returns structured fields suitable for errmap.Error.Context.
func (d Diagnosis) Context() map[string]any {
	store := d.CredentialStore
	if store == "" {
		store = "keyring"
	}
	platform := d.Platform
	if platform == "" {
		platform = currentGOOS()
	}
	reason := d.Reason
	if reason == "" {
		reason = ReasonUnknown
	}
	ctx := map[string]any{
		"credential_store": store,
		"reason":           string(reason),
		"platform":         platform,
	}
	if d.DBusSessionBusSet != nil {
		ctx["dbus_session_bus_set"] = *d.DBusSessionBusSet
	}
	if d.Observed != "" {
		ctx["observed"] = d.Observed
	}
	return ctx
}

// Label returns a concise Chinese description for user-facing error messages.
func (r Reason) Label() string {
	switch r {
	case ReasonNoSessionBus:
		return "未检测到可用的 D-Bus 会话总线"
	case ReasonServiceUnavailable:
		return "系统凭据服务不可用"
	case ReasonLocked:
		return "系统密钥环已锁定或解锁被取消"
	case ReasonPermissionDenied:
		return "系统凭据管理器拒绝访问"
	case ReasonTimeout:
		return "访问系统凭据管理器超时"
	default:
		return ""
	}
}

func isKnownReason(r Reason) bool {
	switch r {
	case ReasonNoSessionBus, ReasonServiceUnavailable, ReasonLocked, ReasonPermissionDenied, ReasonTimeout, ReasonUnknown:
		return true
	default:
		return false
	}
}

// DiagnoseEnvironment performs a non-invasive check of the local credential
// store environment (such as D-Bus session bus hints on Linux).
func DiagnoseEnvironment() EnvironmentCheck {
	platform := currentGOOS()
	check := EnvironmentCheck{
		Platform:        platform,
		CredentialStore: "keyring",
	}
	switch platform {
	case "linux":
		return diagnoseLinuxEnvironment(check)
	case "windows":
		check.Ready = true
		check.Detail = "Windows 凭据管理器"
		return check
	case "darwin":
		check.Ready = true
		check.Detail = "macOS 钥匙串"
		return check
	default:
		check.Ready = false
		check.Reason = ReasonServiceUnavailable
		check.Detail = "当前平台不支持系统凭据管理器：" + platform
		check.Suggestion = SuggestionFor(platform, ReasonServiceUnavailable)
		return check
	}
}

func diagnoseLinuxEnvironment(check EnvironmentCheck) EnvironmentCheck {
	rawAddr, _ := lookupEnv("DBUS_SESSION_BUS_ADDRESS")
	addr := strings.TrimSpace(rawAddr)
	busSet := addr != ""
	check.DBusSessionBusSet = &busSet

	if busSet && !isAutolaunchAddress(addr) {
		paths, hasNonPath, disabled := parseDBusAddresses(addr)
		if disabled {
			exists := false
			check.DBusSocketExists = &exists
			check.Ready = false
			check.Reason = ReasonNoSessionBus
			check.Detail = "DBUS_SESSION_BUS_ADDRESS 被设为 disabled:"
			check.Suggestion = SuggestionFor("linux", ReasonNoSessionBus)
			return check
		}
		if len(paths) > 0 {
			var sawPermErr bool
			for _, p := range paths {
				_, err := statPath(p)
				if err == nil {
					exists := true
					check.DBusSocketExists = &exists
					check.Ready = true
					check.Detail = "已检测到 DBUS_SESSION_BUS_ADDRESS 指向的 D-Bus socket（Secret Service 可用性需实际读写确认）"
					return check
				}
				if errors.Is(err, os.ErrPermission) {
					sawPermErr = true
				}
			}
			if !hasNonPath {
				if sawPermErr {
					check.Ready = false
					check.Reason = ReasonPermissionDenied
					check.Detail = "无权访问 DBUS_SESSION_BUS_ADDRESS 指向的 D-Bus socket"
					check.Suggestion = SuggestionFor("linux", ReasonPermissionDenied)
					return check
				}
				exists := false
				check.DBusSocketExists = &exists
				check.Ready = false
				check.Reason = ReasonNoSessionBus
				check.Detail = "DBUS_SESSION_BUS_ADDRESS 指向的 D-Bus socket 不存在"
				check.Suggestion = SuggestionFor("linux", ReasonNoSessionBus)
				return check
			}
		}
		if hasNonPath {
			check.Ready = true
			check.Detail = "已设置 DBUS_SESSION_BUS_ADDRESS（非路径型总线地址，实际可用性需读写确认）"
			return check
		}
	}

	// DBUS_SESSION_BUS_ADDRESS is unset or autolaunch:; check standard user bus sockets.
	candidates := defaultLinuxDBusSockets()
	var sawPermErr bool
	for _, candidate := range candidates {
		_, err := statPath(candidate)
		if err == nil {
			exists := true
			check.DBusSocketExists = &exists
			check.Ready = true
			check.Detail = "DBUS_SESSION_BUS_ADDRESS 未显式设置，但检测到默认用户 D-Bus socket"
			return check
		}
		if errors.Is(err, os.ErrPermission) {
			sawPermErr = true
		}
	}
	if sawPermErr {
		check.Ready = false
		check.Reason = ReasonPermissionDenied
		check.Detail = "无权访问默认用户 D-Bus socket"
		check.Suggestion = SuggestionFor("linux", ReasonPermissionDenied)
		return check
	}
	exists := false
	check.DBusSocketExists = &exists
	check.Ready = false
	check.Reason = ReasonNoSessionBus
	check.Detail = "未设置 DBUS_SESSION_BUS_ADDRESS，且未检测到默认用户 D-Bus 会话总线 socket"
	check.Suggestion = SuggestionFor("linux", ReasonNoSessionBus)
	return check
}

func isAutolaunchAddress(addr string) bool {
	return addr == "autolaunch:" || strings.HasPrefix(addr, "autolaunch:")
}

func parseDBusAddresses(addr string) (paths []string, hasNonPath bool, disabled bool) {
	entries := strings.Split(addr, ";")
	validEntries := 0
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if entry == "disabled:" || strings.HasPrefix(entry, "disabled:") {
			disabled = true
			continue
		}
		if isAutolaunchAddress(entry) {
			continue
		}
		validEntries++
		if rest, ok := strings.CutPrefix(entry, "unix:"); ok {
			foundPath := false
			for _, kv := range strings.Split(rest, ",") {
				if rawPath, ok := strings.CutPrefix(kv, "path="); ok && rawPath != "" {
					if unescaped, err := url.PathUnescape(rawPath); err == nil {
						paths = append(paths, unescaped)
					} else {
						paths = append(paths, rawPath)
					}
					foundPath = true
				} else if strings.HasPrefix(kv, "abstract=") || strings.HasPrefix(kv, "runtime=") || strings.HasPrefix(kv, "tmpdir=") {
					hasNonPath = true
				}
			}
			if !foundPath && !hasNonPath {
				hasNonPath = true
			}
		} else {
			hasNonPath = true
		}
	}
	if disabled && validEntries == 0 {
		return nil, false, true
	}
	return paths, hasNonPath, false
}

func defaultLinuxDBusSockets() []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	if xdg, ok := lookupEnv("XDG_RUNTIME_DIR"); ok {
		xdg = strings.TrimSpace(xdg)
		if xdg != "" {
			add(path.Join(xdg, "bus"))
			add(path.Join(xdg, "dbus-session"))
		}
	}
	if uid := currentUID(); uid >= 0 {
		base := "/run/user/" + strconv.Itoa(uid)
		add(path.Join(base, "bus"))
		add(path.Join(base, "dbus-session"))
	}
	return candidates
}

// ClassifyKeyringError heuristically classifies a keyring error into a stable
// Diagnosis with observed details and platform-specific recovery suggestions.
func ClassifyKeyringError(err error) Diagnosis {
	env := DiagnoseEnvironment()
	if err == nil {
		return Diagnosis{
			CredentialStore:   "keyring",
			Platform:          env.Platform,
			DBusSessionBusSet: env.DBusSessionBusSet,
		}
	}

	// Preserve an already-classified keyring error's structured diagnosis.
	var appErr *errmap.Error
	if errors.As(err, &appErr) && appErr.Context != nil {
		if diag, ok := diagnosisFromContext(appErr, env); ok {
			return diag
		}
	}

	rawErr := err
	if appErr != nil && appErr.Cause != nil {
		rawErr = appErr.Cause
	}

	reason := classifyReason(rawErr, env)
	observed := sanitizeObserved(rawErr.Error())
	return Diagnosis{
		CredentialStore:   "keyring",
		Reason:            reason,
		Platform:          env.Platform,
		Observed:          observed,
		Suggestion:        SuggestionFor(env.Platform, reason),
		DBusSessionBusSet: env.DBusSessionBusSet,
	}
}

func diagnosisFromContext(appErr *errmap.Error, env EnvironmentCheck) (Diagnosis, bool) {
	store, _ := appErr.Context["credential_store"].(string)
	if store == "" {
		return Diagnosis{}, false
	}
	var reason Reason
	switch v := appErr.Context["reason"].(type) {
	case string:
		reason = Reason(v)
	case Reason:
		reason = v
	}
	if !isKnownReason(reason) {
		return Diagnosis{}, false
	}
	platform, _ := appErr.Context["platform"].(string)
	if platform == "" {
		platform = env.Platform
	}
	observed, _ := appErr.Context["observed"].(string)
	if observed == "" && appErr.Cause != nil {
		observed = sanitizeObserved(appErr.Cause.Error())
	} else {
		observed = sanitizeObserved(observed)
	}
	suggestion := appErr.Suggestion
	if suggestion == "" {
		suggestion = SuggestionFor(platform, reason)
	}
	var dbusSet *bool
	if v, ok := appErr.Context["dbus_session_bus_set"].(bool); ok {
		b := v
		dbusSet = &b
	} else if platform == "linux" {
		dbusSet = env.DBusSessionBusSet
	}
	return Diagnosis{
		CredentialStore:   store,
		Reason:            reason,
		Platform:          platform,
		Observed:          observed,
		Suggestion:        suggestion,
		DBusSessionBusSet: dbusSet,
	}, true
}

func classifyReason(err error, env EnvironmentCheck) Reason {
	lower := strings.ToLower(err.Error())

	// 1. Timeout (our watchdog or D-Bus/backend timeout).
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errKeyringTimeout) ||
		containsAny(lower,
			"org.freedesktop.dbus.error.noreply",
			"org.freedesktop.dbus.error.timeout",
			"did not receive a reply",
			"timed out",
			"timeout",
			"deadline exceeded",
		) {
		return ReasonTimeout
	}

	// 2. Locked keyring / keychain or dismissed unlock prompt.
	if containsAny(lower,
		"org.freedesktop.secret.error.islocked",
		"failed to unlock",
		"locked collection",
		"collection is locked",
		"keyring is locked",
		"keychain is locked",
		"object is locked",
		"is locked",
		"cannot unlock",
		"user interaction is not allowed",
		"interaction not allowed",
		"prompt dismissed",
	) || (strings.Contains(lower, "unlock") && containsAny(lower, "denied", "failed", "dismissed", "canceled", "cancelled")) {
		return ReasonLocked
	}

	// 3. Permission denied / access denied.
	if errors.Is(err, os.ErrPermission) ||
		containsAny(lower,
			"org.freedesktop.dbus.error.accessdenied",
			"access denied",
			"access is denied",
			"permission denied",
			"operation not permitted",
			"read-only file system",
			"not authorized",
			"authorization denied",
			"user canceled",
			"user cancelled",
		) {
		return ReasonPermissionDenied
	}

	// 4. Missing or broken D-Bus session bus.
	if containsAny(lower,
		"dbus_session_bus_address",
		"session bus",
		"dbus-launch",
		"autolaunch",
		"dbus-daemon",
	) {
		return ReasonNoSessionBus
	}

	// 5. Secret Service / platform backend unavailable.
	if errors.Is(err, keyring.ErrUnsupportedPlatform) ||
		containsAny(lower,
			"org.freedesktop.secrets",
			"org.freedesktop.dbus.error.serviceunknown",
			"not provided by any .service files",
			"namehasnoowner",
			"spawn.execfailed",
			"spawn.childexited",
			"spawn.failed",
			"secret service",
			"unsupported platform",
			"no such object path",
			"unknownobject",
			"unknownmethod",
			"service unavailable",
			"not supported",
		) {
		return ReasonServiceUnavailable
	}

	// 6. Linux socket errors and environment-based fallback.
	if env.Platform == "linux" {
		if containsAny(lower, "no such file or directory", "connection refused", "broken pipe", "eof") &&
			(containsAny(lower, "unix", "bus", "socket", "/run/user/") || (!env.Ready && env.Reason == ReasonNoSessionBus)) {
			return ReasonNoSessionBus
		}
		if !env.Ready && (env.Reason == ReasonNoSessionBus || env.Reason == ReasonPermissionDenied) {
			return env.Reason
		}
	}

	return ReasonUnknown
}

// SuggestionFor returns an actionable, platform-specific recovery suggestion
// without ever recommending plaintext credential storage.
func SuggestionFor(platform string, reason Reason) string {
	switch platform {
	case "linux":
		switch reason {
		case ReasonNoSessionBus:
			return "Linux 无头环境未检测到可用的 D-Bus 会话总线。请安装 gnome-keyring、libsecret-tools 与 dbus-user-session（或 dbus-x11），确认 DBUS_SESSION_BUS_ADDRESS 指向有效 socket，或通过 dbus-run-session -- ... 启动会话总线后运行 qqmail-cli auth keyring-test 验证；单次运行可显式使用 --auth-code-env"
		case ReasonServiceUnavailable:
			return "未检测到可用的 Secret Service（org.freedesktop.secrets）。请安装 gnome-keyring、libsecret-tools 与 dbus-user-session（或 dbus-x11），在会话总线内启动 gnome-keyring-daemon 后运行 qqmail-cli auth keyring-test 验证；单次运行可显式使用 --auth-code-env"
		case ReasonLocked:
			return "登录密钥环当前处于锁定状态或解锁提示被取消。请先在当前 D-Bus 会话中解锁 login 密钥环，再运行 qqmail-cli auth keyring-test 验证；单次运行可显式使用 --auth-code-env"
		case ReasonPermissionDenied:
			return "当前用户无权访问或写入系统凭据管理器。请检查当前 D-Bus 会话及 ~/.local/share/keyrings 目录权限后运行 qqmail-cli auth keyring-test 验证；单次运行可显式使用 --auth-code-env"
		case ReasonTimeout:
			return "访问系统凭据管理器超时（无头 Linux 常见于 Secret Service 等待交互式解锁提示）。请确认 D-Bus 会话总线与已解锁的 gnome-keyring-daemon 可用后运行 qqmail-cli auth keyring-test 验证；单次运行可显式使用 --auth-code-env"
		default:
			return "请检查 Linux D-Bus 会话总线与 Secret Service（gnome-keyring、libsecret-tools、dbus-user-session 或 dbus-x11），运行 qqmail-cli auth keyring-test 排查；单次运行可显式使用 --auth-code-env"
		}
	case "darwin":
		switch reason {
		case ReasonLocked:
			return "macOS 钥匙串可能已锁定或当前会话不允许交互弹窗。请先解锁登录钥匙串（如 security unlock-keychain）后重试，或单次显式使用 --auth-code-env"
		case ReasonPermissionDenied:
			return "macOS 钥匙串拒绝了访问或写入。请在“钥匙串访问”中检查权限设置后重试，或单次显式使用 --auth-code-env"
		case ReasonTimeout:
			return "访问 macOS 钥匙串超时（可能在等待授权弹窗）。请在交互会话中确认钥匙串已解锁后重试，或单次显式使用 --auth-code-env"
		default:
			return "请检查 macOS 钥匙串状态及权限，运行 qqmail-cli auth keyring-test 排查；单次运行可显式使用 --auth-code-env"
		}
	case "windows":
		switch reason {
		case ReasonLocked:
			return "Windows 凭据管理器当前可能不可用或会话已锁定。请在交互式用户会话中重试，或单次显式使用 --auth-code-env"
		case ReasonPermissionDenied:
			return "Windows 凭据管理器拒绝了访问或写入。请检查当前用户会话权限后重试，或单次显式使用 --auth-code-env"
		case ReasonTimeout:
			return "访问 Windows 凭据管理器超时。请检查凭据管理器服务状态后重试，或单次显式使用 --auth-code-env"
		default:
			return "请检查 Windows 凭据管理器状态与权限，运行 qqmail-cli auth keyring-test 排查；单次运行可显式使用 --auth-code-env"
		}
	default:
		return "当前平台可能不支持系统凭据管理器；单次运行可显式使用 --auth-code-env（搭配 QQMAIL_CLI_AUTH_CODE）"
	}
}

func keyringErrorMessage(base string, reason Reason) string {
	if label := reason.Label(); label != "" {
		return base + "（" + label + "）"
	}
	return base
}

func sanitizeObserved(raw string) string {
	cleaned := strings.TrimSpace(output.SanitizeLine(output.RedactString(raw)))
	if cleaned == "" {
		return ""
	}
	if utf8.RuneCountInString(cleaned) <= maxObservedRunes {
		return cleaned
	}
	runes := []rune(cleaned)
	return string(runes[:maxObservedRunes-3]) + "..."
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

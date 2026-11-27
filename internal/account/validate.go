package account

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/situker/qqmail-cli/internal/errmap"
)

type DiagnosticSeverity string

const (
	SeverityError   DiagnosticSeverity = "error"
	SeverityWarning DiagnosticSeverity = "warning"
	SeverityInfo    DiagnosticSeverity = "info"
)

type Diagnostic struct {
	Severity DiagnosticSeverity `json:"severity"`
	Code     string             `json:"code"`
	Path     string             `json:"path"`
	Message  string             `json:"message"`
	Line     int                `json:"line,omitempty"`
	Column   int                `json:"column,omitempty"`
}

type ValidationSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

type ValidationReport struct {
	ConfigPath  string            `json:"config_path"`
	Exists      bool              `json:"exists"`
	Valid       bool              `json:"valid"`
	Diagnostics []Diagnostic      `json:"diagnostics"`
	Summary     ValidationSummary `json:"summary"`
}

// AccountPolicy is the effective runtime send/transport policy for one account.
type AccountPolicy struct {
	Name                  string   `json:"name"`
	Email                 string   `json:"email"`
	IsDefault             bool     `json:"is_default"`
	IMAPHost              string   `json:"imap_host"`
	IMAPPort              int      `json:"imap_port"`
	CustomIMAP            bool     `json:"custom_imap"`
	SMTPHost              string   `json:"smtp_host"`
	SMTPPort              int      `json:"smtp_port"`
	CustomSMTP            bool     `json:"custom_smtp"`
	SendAllowlist         []string `json:"send_allowlist"`
	AllowlistReady        bool     `json:"allowlist_ready"`
	SendBlacklist         []string `json:"send_blacklist"`
	AutoSend              bool     `json:"auto_send"`
	AutoSendEffective     bool     `json:"auto_send_effective"`
	DailyAutoLimit        int      `json:"daily_auto_limit"`
	DailyAutoLimitDefault bool     `json:"daily_auto_limit_default"`
	UnknownKeys           []string `json:"unknown_keys"`
	AutoSendBlockReasons  []string `json:"auto_send_block_reasons"`
}

// PolicyPreview is the read-only effective policy summary returned by config policy.
type PolicyPreview struct {
	ConfigPath         string          `json:"config_path"`
	Exists             bool            `json:"exists"`
	DefaultAccount     string          `json:"default_account"`
	Readonly           bool            `json:"readonly"`
	UnknownKeys        []string        `json:"unknown_keys"`
	Accounts           []AccountPolicy `json:"accounts"`
	PolicyBoundaryNote string          `json:"policy_boundary_note"`
}

type strictConfig struct {
	Schema         *int                     `toml:"schema"`
	DefaultAccount *string                  `toml:"default_account"`
	Accounts       map[string]strictAccount `toml:"accounts"`
}

type strictAccount struct {
	Email          *string  `toml:"email"`
	IMAPHost       *string  `toml:"imap_host"`
	IMAPPort       *int     `toml:"imap_port"`
	SMTPHost       *string  `toml:"smtp_host"`
	SMTPPort       *int     `toml:"smtp_port"`
	SendAllowlist  []string `toml:"send_allowlist"`
	AutoSend       *bool    `toml:"auto_send"`
	SendBlacklist  []string `toml:"send_blacklist"`
	DailyAutoLimit *int     `toml:"daily_auto_limit"`
}

var (
	bareKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	dnsLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// publicMailDomains lists public email providers where a *@domain allowlist
// wildcard grants sending permission to arbitrary third-party mailboxes.
var publicMailDomains = map[string]bool{
	"qq.com":         true,
	"foxmail.com":    true,
	"vip.qq.com":     true,
	"gmail.com":      true,
	"163.com":        true,
	"126.com":        true,
	"yeah.net":       true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"icloud.com":     true,
	"me.com":         true,
	"yahoo.com":      true,
	"sina.com":       true,
	"sohu.com":       true,
	"proton.me":      true,
	"protonmail.com": true,
}

// ResolveConfigPath resolves an explicit path or falls back to DefaultPath.
func ResolveConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	resolved, err := DefaultPath()
	if err != nil {
		return "", &errmap.Error{Kind: errmap.Config, Message: "无法确定配置目录", Cause: err}
	}
	return resolved, nil
}

// ValidateFile strictly parses and validates the TOML configuration file at path.
func ValidateFile(path string) (ValidationReport, error) {
	resolved, err := ResolveConfigPath(path)
	if err != nil {
		return ValidationReport{}, err
	}
	raw, readErr := os.ReadFile(resolved)
	if errors.Is(readErr, os.ErrNotExist) {
		report := ValidationReport{ConfigPath: resolved, Exists: false}
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "config_not_found",
			Path:     "",
			Message:  "配置文件不存在",
		})
		return report.finalize(), nil
	}
	if readErr != nil {
		report := ValidationReport{ConfigPath: resolved, Exists: true}
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "config_unreadable",
			Path:     "",
			Message:  fmt.Sprintf("无法读取配置文件：%v", readErr),
		})
		return report.finalize(), nil
	}
	return ValidateBytes(resolved, raw), nil
}

// ValidateBytes strictly parses and validates raw TOML configuration bytes.
func ValidateBytes(configPath string, raw []byte) ValidationReport {
	report := ValidationReport{ConfigPath: configPath, Exists: true}

	var generic map[string]any
	if err := toml.Unmarshal(raw, &generic); err != nil {
		diag := Diagnostic{
			Severity: SeverityError,
			Code:     "invalid_toml",
			Path:     "",
			Message:  "TOML 语法错误：" + err.Error(),
		}
		var derr *toml.DecodeError
		if errors.As(err, &derr) {
			row, col := derr.Position()
			diag.Line = row
			diag.Column = col
			diag.Message = "TOML 语法错误：" + derr.Error()
		}
		report.add(diag)
		return report.finalize()
	}

	var sc strictConfig
	err, strictPanicked := decodeStrictSafe(raw, &sc)
	if strictPanicked {
		// go-toml/v2 v2.2.4 panics in strict.Error when an unknown quoted key
		// contains escape sequences (node.Data is unescaped outside the input
		// slice). Fall back to lenient typed decode + generic unknown-key scan.
		if lenientErr := toml.Unmarshal(raw, &sc); lenientErr != nil {
			var derr *toml.DecodeError
			_ = errors.As(lenientErr, &derr)
			validateGenericStructure(&report, generic, derr, lenientErr)
			return report.finalize()
		}
		collectUnknownFieldsFromGeneric(&report, generic)
	} else if err != nil {
		var strictErr *toml.StrictMissingError
		if errors.As(err, &strictErr) {
			for _, derr := range strictErr.Errors {
				row, col := derr.Position()
				key := derr.Key()
				path := formatKeyPath(key)
				msg := "未知配置字段；拼写错误会导致安全策略（如 send_blacklist）失效"
				if len(key) >= 3 && key[0] == "accounts" {
					msg = "未知账号字段（支持：email, imap_host, imap_port, smtp_host, smtp_port, send_allowlist, auto_send, send_blacklist, daily_auto_limit）；拼写错误会导致安全策略静默失效"
				}
				report.add(Diagnostic{
					Severity: SeverityError,
					Code:     "unknown_field",
					Path:     path,
					Message:  msg,
					Line:     row,
					Column:   col,
				})
			}
		} else {
			// The document is syntactically valid TOML (generic succeeded), so
			// strict decoding failed due to a type or structural mismatch.
			var derr *toml.DecodeError
			_ = errors.As(err, &derr)
			validateGenericStructure(&report, generic, derr, err)
			return report.finalize()
		}
	}

	validateSemantics(&report, sc)
	return report.finalize()
}

func decodeStrictSafe(raw []byte, dst *strictConfig) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst), false
}

func collectUnknownFieldsFromGeneric(report *ValidationReport, generic map[string]any) {
	for _, key := range sortedMapKeys(generic) {
		if !knownTopLevelKeys[key] {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "unknown_field",
				Path:     formatKeyPath([]string{key}),
				Message:  "未知配置字段；拼写错误会导致安全策略（如 send_blacklist）失效",
			})
			continue
		}
		if key != "accounts" {
			continue
		}
		accountsMap, ok := generic[key].(map[string]any)
		if !ok {
			continue
		}
		for _, acctName := range sortedMapKeys(accountsMap) {
			acctTable, ok := accountsMap[acctName].(map[string]any)
			if !ok {
				continue
			}
			for _, field := range sortedMapKeys(acctTable) {
				if knownAccountKeys[field] {
					continue
				}
				report.add(Diagnostic{
					Severity: SeverityError,
					Code:     "unknown_field",
					Path:     formatKeyPath([]string{"accounts", acctName, field}),
					Message:  "未知账号字段（支持：email, imap_host, imap_port, smtp_host, smtp_port, send_allowlist, auto_send, send_blacklist, daily_auto_limit）；拼写错误会导致安全策略静默失效",
				})
			}
		}
	}
}

func (r *ValidationReport) add(d Diagnostic) {
	r.Diagnostics = append(r.Diagnostics, d)
}

func (r ValidationReport) finalize() ValidationReport {
	if r.Diagnostics == nil {
		r.Diagnostics = []Diagnostic{}
	}
	var summary ValidationSummary
	for _, d := range r.Diagnostics {
		switch d.Severity {
		case SeverityError:
			summary.Errors++
		case SeverityWarning:
			summary.Warnings++
		case SeverityInfo:
			summary.Info++
		}
	}
	r.Summary = summary
	r.Valid = summary.Errors == 0
	return r
}

func validateGenericStructure(report *ValidationReport, generic map[string]any, firstErr *toml.DecodeError, rawErr error) {
	row, col := 0, 0
	if firstErr != nil {
		row, col = firstErr.Position()
	}
	attachedPos := false
	addTypeErr := func(path, expected string) {
		d := Diagnostic{
			Severity: SeverityError,
			Code:     "invalid_type",
			Path:     path,
			Message:  fmt.Sprintf("字段类型错误，期望%s", expected),
		}
		if !attachedPos && row > 0 {
			d.Line = row
			d.Column = col
			attachedPos = true
		}
		report.add(d)
	}

	topKeys := sortedMapKeys(generic)
	for _, key := range topKeys {
		val := generic[key]
		switch key {
		case "schema":
			if _, ok := val.(int64); !ok {
				addTypeErr("schema", "整数（例如 1）")
			}
		case "default_account":
			if _, ok := val.(string); !ok {
				addTypeErr("default_account", "字符串")
			}
		case "accounts":
			accountsMap, ok := val.(map[string]any)
			if !ok {
				addTypeErr("accounts", "表（[accounts.<name>]）")
				continue
			}
			for _, acctName := range sortedMapKeys(accountsMap) {
				acctPath := formatKeyPath([]string{"accounts", acctName})
				acctTable, ok := accountsMap[acctName].(map[string]any)
				if !ok {
					addTypeErr(acctPath, "表（[accounts.<name>]）")
					continue
				}
				validateGenericAccount(report, acctName, acctTable, addTypeErr)
			}
		default:
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "unknown_field",
				Path:     formatKeyPath([]string{key}),
				Message:  "未知配置字段；拼写错误会导致安全策略（如 send_blacklist）失效",
			})
		}
	}

	if len(report.Diagnostics) == 0 {
		msg := "配置字段类型错误"
		if rawErr != nil {
			msg = "配置字段类型错误：" + rawErr.Error()
		}
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "invalid_type",
			Path:     "",
			Message:  msg,
			Line:     row,
			Column:   col,
		})
	}
}

func validateGenericAccount(report *ValidationReport, acctName string, table map[string]any, addTypeErr func(path, expected string)) {
	for _, field := range sortedMapKeys(table) {
		val := table[field]
		fieldPath := formatKeyPath([]string{"accounts", acctName, field})
		switch field {
		case "email", "imap_host", "smtp_host":
			if _, ok := val.(string); !ok {
				addTypeErr(fieldPath, "字符串")
			}
		case "imap_port", "smtp_port", "daily_auto_limit":
			if _, ok := val.(int64); !ok {
				addTypeErr(fieldPath, "整数")
			}
		case "auto_send":
			if _, ok := val.(bool); !ok {
				addTypeErr(fieldPath, "布尔值（true/false）")
			}
		case "send_allowlist", "send_blacklist":
			items, ok := val.([]any)
			if !ok {
				addTypeErr(fieldPath, "字符串数组")
				continue
			}
			for i, item := range items {
				if _, ok := item.(string); !ok {
					addTypeErr(fmt.Sprintf("%s[%d]", fieldPath, i), "字符串")
				}
			}
		default:
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "unknown_field",
				Path:     fieldPath,
				Message:  "未知账号字段（支持：email, imap_host, imap_port, smtp_host, smtp_port, send_allowlist, auto_send, send_blacklist, daily_auto_limit）；拼写错误会导致安全策略静默失效",
			})
		}
	}
}

func validateSemantics(report *ValidationReport, sc strictConfig) {
	if sc.Schema == nil {
		report.add(Diagnostic{
			Severity: SeverityWarning,
			Code:     "missing_schema",
			Path:     "schema",
			Message:  fmt.Sprintf("缺少顶层 schema 字段；当前默认按 schema = %d 处理，建议显式写入 schema = %d", ConfigSchema, ConfigSchema),
		})
	} else if *sc.Schema != ConfigSchema {
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "unsupported_schema",
			Path:     "schema",
			Message:  fmt.Sprintf("不支持的配置版本 %d，当前仅支持 schema = %d", *sc.Schema, ConfigSchema),
		})
	}

	if len(sc.Accounts) == 0 {
		if sc.DefaultAccount != nil && strings.TrimSpace(*sc.DefaultAccount) != "" {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "unknown_default_account",
				Path:     "default_account",
				Message:  fmt.Sprintf("default_account 指向不存在的账号 %q", *sc.DefaultAccount),
			})
		}
		report.add(Diagnostic{
			Severity: SeverityWarning,
			Code:     "no_accounts",
			Path:     "accounts",
			Message:  "配置中尚未定义任何账号（可运行 qqmail-cli auth login 添加）",
		})
		return
	}

	if sc.DefaultAccount == nil || strings.TrimSpace(*sc.DefaultAccount) == "" {
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "missing_default_account",
			Path:     "default_account",
			Message:  "已配置账号但未设置 default_account",
		})
	} else if _, ok := sc.Accounts[*sc.DefaultAccount]; !ok {
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "unknown_default_account",
			Path:     "default_account",
			Message:  fmt.Sprintf("default_account 指向不存在的账号 %q", *sc.DefaultAccount),
		})
	}

	names := make([]string, 0, len(sc.Accounts))
	for name := range sc.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		validateAccountSemantics(report, name, sc.Accounts[name])
	}
}

func validateAccountSemantics(report *ValidationReport, name string, acct strictAccount) {
	prefix := formatKeyPath([]string{"accounts", name})
	if !ValidAccountName(name) {
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "invalid_account_name",
			Path:     prefix,
			Message:  "账号名称仅允许字母、数字、点、下划线和连字符，以字母或数字开头，最长 64 字符",
		})
	}

	if acct.Email == nil || strings.TrimSpace(*acct.Email) == "" {
		report.add(Diagnostic{
			Severity: SeverityError,
			Code:     "missing_email",
			Path:     prefix + ".email",
			Message:  "缺少账号邮箱地址 email",
		})
	} else {
		rawEmail := *acct.Email
		trimmed := strings.TrimSpace(rawEmail)
		if !IsValidEmailAddress(trimmed) {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_email",
				Path:     prefix + ".email",
				Message:  "邮箱地址格式无效，应为标准邮箱地址（如 user@qq.com）",
			})
		} else if rawEmail != trimmed {
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "email_whitespace",
				Path:     prefix + ".email",
				Message:  "邮箱地址包含首尾空白字符，建议去除",
			})
		}
	}

	if acct.IMAPHost != nil {
		host := strings.TrimSpace(*acct.IMAPHost)
		if host != "" {
			if !IsValidHost(host) {
				report.add(Diagnostic{
					Severity: SeverityError,
					Code:     "invalid_imap_host",
					Path:     prefix + ".imap_host",
					Message:  "imap_host 格式无效，应为主机名或 IPv4 地址（不含协议或端口）",
				})
			} else if !strings.EqualFold(host, DefaultIMAPHost) {
				report.add(Diagnostic{
					Severity: SeverityWarning,
					Code:     "custom_imap_host",
					Path:     prefix + ".imap_host",
					Message:  fmt.Sprintf("使用了自定义 IMAP 主机 %q（QQ 邮箱默认为 %s），请确认配置未被篡改", host, DefaultIMAPHost),
				})
			}
		}
	}

	if acct.IMAPPort != nil {
		port := *acct.IMAPPort
		switch {
		case port < 0 || port > 65535:
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_imap_port",
				Path:     prefix + ".imap_port",
				Message:  fmt.Sprintf("IMAP 端口 %d 超出有效范围（1-65535）", port),
			})
		case port == 0:
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "imap_port_zero_default",
				Path:     prefix + ".imap_port",
				Message:  fmt.Sprintf("imap_port = 0 将回退到默认端口 %d，建议省略或显式设置有效端口", DefaultIMAPPort),
			})
		case port != DefaultIMAPPort:
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "custom_imap_port",
				Path:     prefix + ".imap_port",
				Message:  fmt.Sprintf("使用了非默认 IMAP 端口 %d（QQ 邮箱默认为 %d），请确认配置未被篡改", port, DefaultIMAPPort),
			})
		}
	}

	if acct.SMTPHost != nil {
		host := strings.TrimSpace(*acct.SMTPHost)
		if host != "" {
			if !IsValidHost(host) {
				report.add(Diagnostic{
					Severity: SeverityError,
					Code:     "invalid_smtp_host",
					Path:     prefix + ".smtp_host",
					Message:  "smtp_host 格式无效，应为主机名或 IPv4 地址（不含协议或端口）",
				})
			} else if !strings.EqualFold(host, DefaultSMTPHost) {
				report.add(Diagnostic{
					Severity: SeverityWarning,
					Code:     "custom_smtp_host",
					Path:     prefix + ".smtp_host",
					Message:  fmt.Sprintf("使用了自定义 SMTP 主机 %q（QQ 邮箱默认为 %s），请确认配置未被篡改", host, DefaultSMTPHost),
				})
			}
		}
	}

	if acct.SMTPPort != nil {
		port := *acct.SMTPPort
		switch {
		case port < 0 || port > 65535:
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_smtp_port",
				Path:     prefix + ".smtp_port",
				Message:  fmt.Sprintf("SMTP 端口 %d 超出有效范围（1-65535）", port),
			})
		case port == 0:
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "smtp_port_zero_default",
				Path:     prefix + ".smtp_port",
				Message:  fmt.Sprintf("smtp_port = 0 将回退到默认端口 %d，建议省略或显式设置有效端口", DefaultSMTPPort),
			})
		case port != DefaultSMTPPort:
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "custom_smtp_port",
				Path:     prefix + ".smtp_port",
				Message:  fmt.Sprintf("使用了非默认 SMTP 端口 %d（QQ 邮箱默认为 %d，备选 587），请确认配置未被篡改", port, DefaultSMTPPort),
			})
		}
	}

	validAllowlist := make([]string, 0, len(acct.SendAllowlist))
	seenAllow := make(map[string]bool, len(acct.SendAllowlist))
	for i, rawEntry := range acct.SendAllowlist {
		entryPath := fmt.Sprintf("%s.send_allowlist[%d]", prefix, i)
		trimmed := strings.TrimSpace(rawEntry)
		if !IsValidAllowlistEntry(trimmed) {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_allowlist_entry",
				Path:     entryPath,
				Message:  "send_allowlist 条目格式无效；仅支持精确邮箱地址或 *@domain 通配符",
			})
			continue
		}
		if rawEntry != trimmed {
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "allowlist_entry_whitespace",
				Path:     entryPath,
				Message:  "send_allowlist 条目包含首尾空白字符，建议去除",
			})
		}
		norm := strings.ToLower(trimmed)
		if seenAllow[norm] {
			report.add(Diagnostic{
				Severity: SeverityInfo,
				Code:     "duplicate_allowlist_entry",
				Path:     entryPath,
				Message:  fmt.Sprintf("send_allowlist 中存在重复条目 %q", trimmed),
			})
		} else {
			seenAllow[norm] = true
			validAllowlist = append(validAllowlist, norm)
		}
		if strings.HasPrefix(norm, "*@") && publicMailDomains[norm[2:]] {
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "broad_allowlist_wildcard",
				Path:     entryPath,
				Message:  fmt.Sprintf("通配符 %q 覆盖公共邮箱域，权限范围过宽，建议收窄为具体收件人地址", trimmed),
			})
		}
	}

	seenBlack := make(map[string]bool, len(acct.SendBlacklist))
	for i, rawEntry := range acct.SendBlacklist {
		entryPath := fmt.Sprintf("%s.send_blacklist[%d]", prefix, i)
		trimmed := strings.TrimSpace(rawEntry)
		if strings.Contains(trimmed, "*") {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "blacklist_wildcard_unsupported",
				Path:     entryPath,
				Message:  "send_blacklist 仅支持精确邮箱地址，不支持通配符；该条目在 autosend 时不会生效",
			})
			continue
		}
		if !IsValidEmailAddress(trimmed) {
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_blacklist_entry",
				Path:     entryPath,
				Message:  "send_blacklist 条目格式无效；必须为精确邮箱地址",
			})
			continue
		}
		if rawEntry != trimmed {
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "blacklist_entry_whitespace",
				Path:     entryPath,
				Message:  "send_blacklist 条目包含首尾空白字符，建议去除",
			})
		}
		norm := strings.ToLower(trimmed)
		if seenBlack[norm] {
			report.add(Diagnostic{
				Severity: SeverityInfo,
				Code:     "duplicate_blacklist_entry",
				Path:     entryPath,
				Message:  fmt.Sprintf("send_blacklist 中存在重复条目 %q", trimmed),
			})
		} else {
			seenBlack[norm] = true
		}
		if !AllowlistMatches(norm, validAllowlist) {
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "redundant_blacklist_entry",
				Path:     entryPath,
				Message:  fmt.Sprintf("send_blacklist 中的 %q 未命中 send_allowlist，当前无实际作用（未在白名单中的地址本就会被拒发）", trimmed),
			})
		}
	}

	autoSend := acct.AutoSend != nil && *acct.AutoSend
	if autoSend && len(acct.SendAllowlist) == 0 {
		report.add(Diagnostic{
			Severity: SeverityWarning,
			Code:     "autosend_empty_allowlist",
			Path:     prefix + ".auto_send",
			Message:  "已开启 auto_send，但 send_allowlist 为空；所有发送仍会被白名单门禁拒绝",
		})
	}
	if !autoSend && len(acct.SendBlacklist) > 0 {
		report.add(Diagnostic{
			Severity: SeverityInfo,
			Code:     "blacklist_without_autosend",
			Path:     prefix + ".send_blacklist",
			Message:  "已配置 send_blacklist，但当前未开启 auto_send（send_blacklist 仅在自动发送时生效）",
		})
	}

	if acct.DailyAutoLimit != nil {
		limit := *acct.DailyAutoLimit
		switch {
		case limit < 0:
			report.add(Diagnostic{
				Severity: SeverityError,
				Code:     "invalid_daily_auto_limit",
				Path:     prefix + ".daily_auto_limit",
				Message:  fmt.Sprintf("daily_auto_limit 不能为负数（当前为 %d）；运行时会归一化为默认值 %d，请改为正整数或省略该字段", limit, DefaultDailyAutoLimit),
			})
		case limit == 0:
			report.add(Diagnostic{
				Severity: SeverityWarning,
				Code:     "daily_auto_limit_normalized",
				Path:     prefix + ".daily_auto_limit",
				Message:  fmt.Sprintf("daily_auto_limit = 0 不表示无限制，运行时会归一化为默认值 %d；建议省略或填写正整数", DefaultDailyAutoLimit),
			})
		}
	}
}

// AllowlistMatches matches the exact runtime recipient matcher in
// internal/sendmail/message.go: case-insensitive trimmed exact address or
// *@domain wildcard against a single-@ address.
func AllowlistMatches(address string, allowlist []string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == address {
			return true
		}
		if strings.HasPrefix(entry, "*@") && strings.HasSuffix(address, entry[1:]) && strings.Count(address, "@") == 1 {
			return true
		}
	}
	return false
}

// IsValidEmailAddress checks that value is a bare RFC 5322 addr-spec with a
// single '@', no wildcard characters, no display name, and a valid domain.
func IsValidEmailAddress(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n<>") || strings.Contains(value, "*") {
		return false
	}
	if strings.Count(value, "@") != 1 {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return false
	}
	parts := strings.SplitN(value, "@", 2)
	if parts[0] == "" {
		return false
	}
	return isValidDomain(parts[1])
}

// IsValidAllowlistEntry checks whether value is either a valid bare email
// address or a valid *@domain wildcard.
func IsValidAllowlistEntry(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n<>") {
		return false
	}
	if strings.HasPrefix(value, "*@") {
		return isValidDomain(value[2:])
	}
	if strings.Contains(value, "*") {
		return false
	}
	return IsValidEmailAddress(value)
}

// IsValidHost checks whether value is a valid hostname (including localhost)
// or IPv4 literal without a scheme, path, port, or control characters.
func IsValidHost(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n:/\\@") {
		return false
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.To4() != nil
	}
	if strings.EqualFold(value, "localhost") {
		return true
	}
	return isValidDomain(value)
}

func isValidDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// BuildPolicyPreview projects the effective runtime enforcement policy for all
// accounts (or one filtered account) from a loaded Config. It never touches
// credentials or the network.
func BuildPolicyPreview(cfg *Config, configPath string, accountFilter string, readonly bool) (PolicyPreview, error) {
	preview := PolicyPreview{
		ConfigPath:         configPath,
		Exists:             true,
		DefaultAccount:     cfg.DefaultAccount,
		Readonly:           readonly,
		UnknownKeys:        nonNilStrings(cfg.UnknownKeys),
		Accounts:           []AccountPolicy{},
		PolicyBoundaryNote: PolicyBoundaryNote,
	}

	var names []string
	if accountFilter != "" {
		if _, ok := cfg.Accounts[accountFilter]; !ok {
			return PolicyPreview{}, &errmap.Error{
				Kind:       errmap.Config,
				Message:    "账号不存在：" + accountFilter,
				Suggestion: "运行 qqmail-cli account list 或 qqmail-cli config policy 查看已配置账号",
			}
		}
		names = []string{accountFilter}
	} else {
		names = make([]string, 0, len(cfg.Accounts))
		for name := range cfg.Accounts {
			names = append(names, name)
		}
		sort.Strings(names)
	}

	for _, name := range names {
		rawAcct := cfg.Accounts[name]
		named, err := cfg.Resolve(name)
		if err != nil {
			return PolicyPreview{}, err
		}
		preview.Accounts = append(preview.Accounts, accountPolicyFrom(named, rawAcct, readonly))
	}
	return preview, nil
}

func accountPolicyFrom(named Named, raw Account, readonly bool) AccountPolicy {
	allowlist := nonNilStrings(named.SendAllowlist)
	blacklist := nonNilStrings(named.SendBlacklist)
	unknown := nonNilStrings(named.UnknownKeys)

	blockReasons := []string{}
	if named.AutoSend {
		if readonly {
			blockReasons = append(blockReasons, "readonly")
		}
		if len(allowlist) == 0 {
			blockReasons = append(blockReasons, "empty_allowlist")
		}
		if len(unknown) > 0 {
			blockReasons = append(blockReasons, "unknown_keys")
		}
	}

	return AccountPolicy{
		Name:                  named.Name,
		Email:                 named.Email,
		IsDefault:             named.IsDefault,
		IMAPHost:              named.IMAPHost,
		IMAPPort:              named.IMAPPort,
		CustomIMAP:            !strings.EqualFold(named.IMAPHost, DefaultIMAPHost) || named.IMAPPort != DefaultIMAPPort,
		SMTPHost:              named.SMTPHost,
		SMTPPort:              named.SMTPPort,
		CustomSMTP:            !strings.EqualFold(named.SMTPHost, DefaultSMTPHost) || named.SMTPPort != DefaultSMTPPort,
		SendAllowlist:         allowlist,
		AllowlistReady:        len(allowlist) > 0,
		SendBlacklist:         blacklist,
		AutoSend:              named.AutoSend,
		AutoSendEffective:     named.AutoSend && len(blockReasons) == 0,
		DailyAutoLimit:        named.DailyAutoLimit,
		DailyAutoLimitDefault: raw.DailyAutoLimit <= 0,
		UnknownKeys:           unknown,
		AutoSendBlockReasons:  blockReasons,
	}
}

func formatKeyPath(key []string) string {
	parts := make([]string, len(key))
	for i, seg := range key {
		if bareKeyPattern.MatchString(seg) {
			parts[i] = seg
		} else {
			parts[i] = fmt.Sprintf("%q", seg)
		}
	}
	return strings.Join(parts, ".")
}

func appendUniqueSorted(slice []string, item string) []string {
	for _, existing := range slice {
		if existing == item {
			return slice
		}
	}
	slice = append(slice, item)
	sort.Strings(slice)
	return slice
}

func mergeUniqueSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

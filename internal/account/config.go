package account

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/situker/qqmail-cli/internal/errmap"
)

const (
	ConfigSchema    = 1
	DefaultIMAPHost = "imap.qq.com"
	DefaultIMAPPort = 993
	DefaultSMTPHost = "smtp.qq.com"
	DefaultSMTPPort = 465

	// PolicyBoundaryNote documents the honest threat boundary of the TOML config:
	// the CLI never exposes a write path for send_allowlist, send_blacklist, or
	// auto_send, yet the same OS user owns and can edit the file directly.
	PolicyBoundaryNote = "send_allowlist、send_blacklist 与 auto_send 没有 CLI 写入入口，需由人工编辑配置文件；但同一操作系统用户可直接修改该 TOML 文件，因此这些门禁用于防误操作和防不可信邮件内容诱导，不是不可绕过的操作系统安全边界。"
)

// DefaultDailyAutoLimit is the per-day autosend cap whenever an account's
// daily_auto_limit is missing or non-positive — there is no "0 = unlimited"
// reading. cli/autosend.go re-applies it as defense in depth for directly
// constructed accounts; keep the single constant as the only source.
const DefaultDailyAutoLimit = 50

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidAccountName reports whether name conforms to the account alias rules.
func ValidAccountName(name string) bool {
	return validName.MatchString(name)
}

type Account struct {
	Email         string   `toml:"email" json:"email"`
	IMAPHost      string   `toml:"imap_host,omitempty" json:"imap_host"`
	IMAPPort      int      `toml:"imap_port,omitempty" json:"imap_port"`
	SMTPHost      string   `toml:"smtp_host,omitempty" json:"smtp_host"`
	SMTPPort      int      `toml:"smtp_port,omitempty" json:"smtp_port"`
	SendAllowlist []string `toml:"send_allowlist,omitempty" json:"send_allowlist"`
	// Autosend settings (edited by hand in config.toml; the CLI has no command
	// that writes them). DailyAutoLimit ≤0 is normalized to the 50-per-day
	// default when the account is resolved into Named.
	AutoSend       bool     `toml:"auto_send,omitempty" json:"auto_send,omitempty"`
	SendBlacklist  []string `toml:"send_blacklist,omitempty" json:"send_blacklist,omitempty"`
	DailyAutoLimit int      `toml:"daily_auto_limit,omitempty" json:"daily_auto_limit,omitempty"`
	// UnknownKeys records unrecognized TOML key paths observed under this
	// account table during lenient Load so security-sensitive paths (such as
	// autosend) can fail closed without altering existing JSON/TOML outputs.
	UnknownKeys []string `toml:"-" json:"-"`
}

func (a Account) Host() string {
	if a.IMAPHost == "" {
		return DefaultIMAPHost
	}
	return a.IMAPHost
}

func (a Account) Port() int {
	if a.IMAPPort == 0 {
		return DefaultIMAPPort
	}
	return a.IMAPPort
}

func (a Account) Address() string { return fmt.Sprintf("%s:%d", a.Host(), a.Port()) }

func (a Account) SMTPAddress() string {
	host := a.SMTPHost
	if host == "" {
		host = DefaultSMTPHost
	}
	port := a.SMTPPort
	if port == 0 {
		port = DefaultSMTPPort
	}
	return fmt.Sprintf("%s:%d", host, port)
}

type Config struct {
	Schema         int                `toml:"schema" json:"schema"`
	DefaultAccount string             `toml:"default_account" json:"default_account"`
	Accounts       map[string]Account `toml:"accounts" json:"accounts"`
	// UnknownKeys records unrecognized top-level TOML key paths observed during
	// lenient Load (for example a misplaced send_blacklist outside [accounts]).
	UnknownKeys []string `toml:"-" json:"-"`
}

type Named struct {
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	IMAPHost      string   `json:"imap_host"`
	IMAPPort      int      `json:"imap_port"`
	SMTPHost      string   `json:"smtp_host"`
	SMTPPort      int      `json:"smtp_port"`
	SendAllowlist []string `json:"send_allowlist"`
	AutoSend      bool     `json:"auto_send"`
	// SendBlacklist holds the addresses autosend must never fire for; hits
	// fall back to the interactive TTY gate.
	SendBlacklist  []string `json:"send_blacklist"`
	DailyAutoLimit int      `json:"daily_auto_limit"`
	IsDefault      bool     `json:"is_default"`
	// UnknownKeys combines top-level and account-level unrecognized TOML key
	// paths loaded from disk. Omitted from JSON so existing command outputs stay
	// unchanged; autosend checks it to fail closed on typos like send_blaklist.
	UnknownKeys []string `json:"-"`
}

func DefaultPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "qqmail-cli", "config.toml"), nil
}

func Load(path string) (*Config, string, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, "", &errmap.Error{Kind: errmap.Config, Message: "无法确定配置目录", Cause: err}
		}
	}
	cfg := &Config{Schema: ConfigSchema, Accounts: make(map[string]Account)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return nil, path, &errmap.Error{Kind: errmap.Config, Message: "无法读取配置文件", Cause: err}
	}
	if err := decodeLenientWithUnknowns(raw, cfg); err != nil {
		return nil, path, &errmap.Error{Kind: errmap.Config, Message: "配置文件格式损坏", Cause: err}
	}
	if cfg.Schema != ConfigSchema {
		return nil, path, &errmap.Error{Kind: errmap.Config, Message: fmt.Sprintf("不支持的配置版本 %d", cfg.Schema)}
	}
	if cfg.Accounts == nil {
		cfg.Accounts = make(map[string]Account)
	}
	return cfg, path, nil
}

func decodeLenientWithUnknowns(raw []byte, cfg *Config) error {
	if err := toml.Unmarshal(raw, cfg); err != nil {
		return err
	}
	// Detect unknown keys from a generic map decode rather than go-toml's
	// DisallowUnknownFields(), because go-toml/v2 v2.2.4 panics in
	// strict.Error/SubsliceOffset when an unknown quoted key contains escape
	// sequences (e.g. "\u001b").
	var generic map[string]any
	if err := toml.Unmarshal(raw, &generic); err == nil {
		recordUnknownKeysFromGeneric(cfg, generic)
	}
	return nil
}

var (
	knownTopLevelKeys = map[string]bool{
		"schema":          true,
		"default_account": true,
		"accounts":        true,
	}
	knownAccountKeys = map[string]bool{
		"email":            true,
		"imap_host":        true,
		"imap_port":        true,
		"smtp_host":        true,
		"smtp_port":        true,
		"send_allowlist":   true,
		"auto_send":        true,
		"send_blacklist":   true,
		"daily_auto_limit": true,
	}
)

func recordUnknownKeysFromGeneric(cfg *Config, generic map[string]any) {
	if cfg.Accounts == nil {
		cfg.Accounts = make(map[string]Account)
	}
	for _, key := range sortedMapKeys(generic) {
		if !knownTopLevelKeys[key] {
			cfg.UnknownKeys = appendUniqueSorted(cfg.UnknownKeys, formatKeyPath([]string{key}))
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
				acct := cfg.Accounts[acctName]
				acct.UnknownKeys = appendUniqueSorted(acct.UnknownKeys, formatKeyPath([]string{"accounts", acctName, field}))
				cfg.Accounts[acctName] = acct
			}
		}
	}
}

func (c *Config) Save(path string) error {
	if path == "" {
		return &errmap.Error{Kind: errmap.Config, Message: "配置路径为空"}
	}
	c.Schema = ConfigSchema
	raw, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (c *Config) Put(name string, value Account) error {
	if !validName.MatchString(name) {
		return &errmap.Error{Kind: errmap.Usage, Message: "账号名称仅允许字母、数字、点、下划线和连字符，最长 64 字符"}
	}
	value.Email = strings.TrimSpace(value.Email)
	if value.Email == "" || !strings.Contains(value.Email, "@") {
		return &errmap.Error{Kind: errmap.Usage, Message: "邮箱地址格式无效"}
	}
	if value.IMAPPort < 0 || value.IMAPPort > 65535 {
		return &errmap.Error{Kind: errmap.Usage, Message: "IMAP 端口无效"}
	}
	if value.SMTPPort < 0 || value.SMTPPort > 65535 {
		return &errmap.Error{Kind: errmap.Usage, Message: "SMTP 端口无效"}
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]Account)
	}
	c.Accounts[name] = value
	if c.DefaultAccount == "" {
		c.DefaultAccount = name
	}
	return nil
}

func (c *Config) Remove(name string) { delete(c.Accounts, name) }

func (c *Config) Use(name string) error {
	if _, ok := c.Accounts[name]; !ok {
		return &errmap.Error{Kind: errmap.Config, Message: "账号不存在：" + name}
	}
	c.DefaultAccount = name
	return nil
}

func (c *Config) Resolve(name string) (Named, error) {
	if name == "" {
		name = c.DefaultAccount
	}
	value, ok := c.Accounts[name]
	if !ok || name == "" {
		return Named{}, &errmap.Error{Kind: errmap.Config, Message: "尚未配置账号", Suggestion: "先运行 qqmail-cli auth login"}
	}
	smtpHost := value.SMTPHost
	if smtpHost == "" {
		smtpHost = DefaultSMTPHost
	}
	smtpPort := value.SMTPPort
	if smtpPort == 0 {
		smtpPort = DefaultSMTPPort
	}
	dailyAutoLimit := value.DailyAutoLimit
	if dailyAutoLimit <= 0 {
		dailyAutoLimit = DefaultDailyAutoLimit
	}
	unknownKeys := mergeUniqueSorted(c.UnknownKeys, value.UnknownKeys)
	return Named{
		Name:           name,
		Email:          value.Email,
		IMAPHost:       value.Host(),
		IMAPPort:       value.Port(),
		SMTPHost:       smtpHost,
		SMTPPort:       smtpPort,
		SendAllowlist:  append([]string(nil), value.SendAllowlist...),
		AutoSend:       value.AutoSend,
		SendBlacklist:  append([]string(nil), value.SendBlacklist...),
		DailyAutoLimit: dailyAutoLimit,
		IsDefault:      name == c.DefaultAccount,
		UnknownKeys:    unknownKeys,
	}, nil
}

func (c *Config) List() []Named {
	names := make([]string, 0, len(c.Accounts))
	for name := range c.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Named, 0, len(names))
	for _, name := range names {
		named, _ := c.Resolve(name)
		result = append(result, named)
	}
	return result
}

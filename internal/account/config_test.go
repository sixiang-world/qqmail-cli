package account

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRoundTripContainsNoSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &Config{Accounts: map[string]Account{}}
	if err := cfg.Put("personal", Account{Email: "user@qq.com"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("second save must replace config safely: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "auth_code") || strings.Contains(string(raw), "abcdefghijklmnop") {
		t.Fatalf("config contains secret field: %s", raw)
	}
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.Resolve("")
	if err != nil || got.Email != "user@qq.com" || got.IMAPHost != "imap.qq.com" || got.IMAPPort != 993 {
		t.Fatalf("unexpected account: %#v, %v", got, err)
	}
}

func TestInvalidAccountName(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Put("../escape", Account{Email: "user@qq.com"}); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestConfigSupportsUnicodePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "中文用户", "邮箱配置.toml")
	cfg := &Config{Accounts: map[string]Account{}}
	if err := cfg.Put("工作邮箱", Account{Email: "user@qq.com"}); err == nil {
		t.Fatal("non-ASCII account aliases are intentionally rejected for stable CLI use")
	}
	if err := cfg.Put("work", Account{Email: "user@qq.com"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

// The autosend settings (auto_send/send_blacklist/daily_auto_limit) parse from
// the account's TOML section, copy into Named, and the daily limit normalizes:
// missing or ≤0 always means the 50-per-day default — there is no "0 =
// unlimited" reading.
func TestAutoSendSettingsParseNormalizeAndDefault(t *testing.T) {
	write := func(t *testing.T, body string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		raw := "schema = 1\ndefault_account = \"personal\"\n[accounts.personal]\nemail = \"user@qq.com\"\nsend_allowlist = [\"reader@example.com\"]\n" + body
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Run("explicit values parse", func(t *testing.T) {
		cfg := write(t, "auto_send = true\nsend_blacklist = [\"bot@trap.example\"]\ndaily_auto_limit = 30\n")
		named, err := cfg.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if !named.AutoSend || named.DailyAutoLimit != 30 || len(named.SendBlacklist) != 1 || named.SendBlacklist[0] != "bot@trap.example" {
			t.Fatalf("unexpected named account: %+v", named)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		cfg := write(t, "")
		named, err := cfg.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if named.AutoSend || len(named.SendBlacklist) != 0 || named.DailyAutoLimit != 50 {
			t.Fatalf("defaults wrong: auto=%v blacklist=%v limit=%d", named.AutoSend, named.SendBlacklist, named.DailyAutoLimit)
		}
	})
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("non-positive limit %d normalizes to 50", limit), func(t *testing.T) {
			cfg := write(t, fmt.Sprintf("auto_send = true\ndaily_auto_limit = %d\n", limit))
			named, err := cfg.Resolve("")
			if err != nil {
				t.Fatal(err)
			}
			if named.DailyAutoLimit != 50 {
				t.Fatalf("daily_auto_limit %d must normalize to 50, got %d", limit, named.DailyAutoLimit)
			}
		})
	}
	t.Run("round trip through Save keeps the settings", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		cfg := &Config{Schema: ConfigSchema, DefaultAccount: "personal", Accounts: map[string]Account{
			"personal": {Email: "user@qq.com", AutoSend: true, SendBlacklist: []string{"bot@trap.example"}, DailyAutoLimit: 30},
		}}
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		loaded, _, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		named, err := loaded.Resolve("")
		if err != nil {
			t.Fatal(err)
		}
		if !named.AutoSend || named.DailyAutoLimit != 30 || len(named.SendBlacklist) != 1 {
			t.Fatalf("autosend settings lost across save/load: %+v", named)
		}
	})
}

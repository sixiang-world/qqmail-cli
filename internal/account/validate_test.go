package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/errmap"
)

func writeTempConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasDiagCode(report ValidationReport, sev DiagnosticSeverity, code string) bool {
	for _, d := range report.Diagnostics {
		if d.Severity == sev && d.Code == code {
			return true
		}
	}
	return false
}

func findDiag(report ValidationReport, code string) (Diagnostic, bool) {
	for _, d := range report.Diagnostics {
		if d.Code == code {
			return d, true
		}
	}
	return Diagnostic{}, false
}

func TestValidateFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.toml")
	report, err := ValidateFile(missing)
	if err != nil {
		t.Fatal(err)
	}
	if report.Exists || report.Valid || report.Summary.Errors != 1 {
		t.Fatalf("missing config must not be valid: %+v", report)
	}
	if !hasDiagCode(report, SeverityError, "config_not_found") {
		t.Fatalf("expected config_not_found, got %+v", report.Diagnostics)
	}
}

func TestValidateBytesCleanConfig(t *testing.T) {
	raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["reader@example.com", "*@company.example"]
auto_send = true
send_blacklist = ["boss@company.example"]
daily_auto_limit = 20
`
	report := ValidateBytes("config.toml", []byte(raw))
	if !report.Valid || report.Summary.Errors != 0 || report.Summary.Warnings != 0 || report.Summary.Info != 0 {
		t.Fatalf("clean config should have zero diagnostics: %+v", report)
	}
}

func TestValidateBytesSyntaxAndTypes(t *testing.T) {
	t.Run("invalid TOML syntax reports line and column", func(t *testing.T) {
		raw := "schema = 1\n[accounts.personal\nemail = \"user@qq.com\"\n"
		report := ValidateBytes("config.toml", []byte(raw))
		if report.Valid {
			t.Fatal("invalid TOML must fail validation")
		}
		d, ok := findDiag(report, "invalid_toml")
		if !ok || d.Severity != SeverityError || d.Line == 0 {
			t.Fatalf("expected invalid_toml with line number, got %+v", report.Diagnostics)
		}
	})

	t.Run("wrong value types and unknown fields reported together", func(t *testing.T) {
		raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
imap_port = "993"
auto_send = "yes"
send_allowlist = [123]
send_blaklist = ["boss@example.com"]
`
		report := ValidateBytes("config.toml", []byte(raw))
		if report.Valid {
			t.Fatal("type mismatches must fail validation")
		}
		for _, wantPath := range []string{
			"accounts.personal.imap_port",
			"accounts.personal.auto_send",
			"accounts.personal.send_allowlist[0]",
		} {
			found := false
			for _, d := range report.Diagnostics {
				if d.Code == "invalid_type" && d.Path == wantPath {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("missing invalid_type for %s in %+v", wantPath, report.Diagnostics)
			}
		}
		if !hasDiagCode(report, SeverityError, "unknown_field") {
			t.Errorf("expected unknown_field alongside type error: %+v", report.Diagnostics)
		}
	})
}

func TestValidateBytesUnknownFieldsAndLoadRecording(t *testing.T) {
	raw := `
schema = 1
default_account = "work.qq"
send_blacklist = ["top@example.com"]

[accounts."work.qq"]
email = "user@qq.com"
send_allowlist = ["reader@example.com"]
auto_send = true
send_blaklist = ["boss@example.com"]
`
	path := writeTempConfig(t, "config.toml", raw)

	report, err := ValidateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid {
		t.Fatal("unknown fields must fail strict validation")
	}
	var paths []string
	for _, d := range report.Diagnostics {
		if d.Code == "unknown_field" {
			if d.Line == 0 {
				t.Errorf("unknown_field %s missing line number", d.Path)
			}
			paths = append(paths, d.Path)
		}
	}
	if !slices.Contains(paths, "send_blacklist") || !slices.Contains(paths, `accounts."work.qq".send_blaklist`) {
		t.Fatalf("unexpected unknown_field paths: %v (all=%+v)", paths, report.Diagnostics)
	}

	// Lenient Load must still succeed for ordinary read commands while recording
	// both top-level and account-level unknown keys for fail-closed autosend.
	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("lenient Load must succeed with unknown keys: %v", err)
	}
	named, err := cfg.Resolve("work.qq")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(named.UnknownKeys, "send_blacklist") || !slices.Contains(named.UnknownKeys, `accounts."work.qq".send_blaklist`) {
		t.Fatalf("Named.UnknownKeys did not capture unknown paths: %v", named.UnknownKeys)
	}
	// JSON serialization of Named must remain unchanged (no unknown_keys leak).
	encoded, err := json.Marshal(named)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "unknown") {
		t.Fatalf("Named JSON must not expose UnknownKeys: %s", encoded)
	}

	// Quoted unknown keys containing escape sequences trigger a panic in
	// go-toml/v2 v2.2.4's StrictMissingError; both ValidateBytes and Load must
	// handle them cleanly without panicking.
	escapedRaw := "schema = 1\ndefault_account = \"personal\"\n[accounts.personal]\nemail = \"user@qq.com\"\n\"bad\\u001b[31mkey\" = true\n"
	escapedPath := writeTempConfig(t, "escaped.toml", escapedRaw)
	escapedReport, err := ValidateFile(escapedPath)
	if err != nil {
		t.Fatal(err)
	}
	if escapedReport.Valid || !hasDiagCode(escapedReport, SeverityError, "unknown_field") {
		t.Fatalf("escaped unknown key must report unknown_field without panic: %+v", escapedReport)
	}
	escapedCfg, _, err := Load(escapedPath)
	if err != nil {
		t.Fatalf("Load on escaped unknown key failed: %v", err)
	}
	escapedNamed, err := escapedCfg.Resolve("personal")
	if err != nil || len(escapedNamed.UnknownKeys) != 1 {
		t.Fatalf("Load did not record escaped unknown key: named=%+v err=%v", escapedNamed, err)
	}
}

func TestValidateBytesSchemaAccountsAndEndpoints(t *testing.T) {
	t.Run("unsupported schema and missing/unknown default account", func(t *testing.T) {
		raw := `
schema = 2
default_account = "ghost"

[accounts."../escape"]
email = "not-an-email"
imap_host = "imap://bad.example"
imap_port = 70000
smtp_host = "smtp.example:465"
smtp_port = -1
`
		report := ValidateBytes("config.toml", []byte(raw))
		if report.Valid {
			t.Fatal("invalid config must fail")
		}
		for _, code := range []string{
			"unsupported_schema",
			"unknown_default_account",
			"invalid_account_name",
			"invalid_email",
			"invalid_imap_host",
			"invalid_imap_port",
			"invalid_smtp_host",
			"invalid_smtp_port",
		} {
			if !hasDiagCode(report, SeverityError, code) {
				t.Errorf("expected error code %q in %+v", code, report.Diagnostics)
			}
		}
	})

	t.Run("missing default account when accounts exist", func(t *testing.T) {
		raw := `
[accounts.personal]
email = "user@qq.com"
`
		report := ValidateBytes("config.toml", []byte(raw))
		if report.Valid {
			t.Fatal("missing default_account must fail")
		}
		if !hasDiagCode(report, SeverityWarning, "missing_schema") {
			t.Errorf("expected missing_schema warning: %+v", report.Diagnostics)
		}
		if !hasDiagCode(report, SeverityError, "missing_default_account") {
			t.Errorf("expected missing_default_account error: %+v", report.Diagnostics)
		}
	})

	t.Run("custom hosts and unusual ports warn but remain valid", func(t *testing.T) {
		raw := `
schema = 1
default_account = "local"

[accounts.local]
email = "user@qq.com"
imap_host = "127.0.0.1"
imap_port = 1993
smtp_host = "localhost"
smtp_port = 587
`
		report := ValidateBytes("config.toml", []byte(raw))
		if !report.Valid {
			t.Fatalf("custom valid endpoints should warn, not error: %+v", report.Diagnostics)
		}
		for _, code := range []string{"custom_imap_host", "custom_imap_port", "custom_smtp_host", "custom_smtp_port"} {
			if !hasDiagCode(report, SeverityWarning, code) {
				t.Errorf("expected warning %q in %+v", code, report.Diagnostics)
			}
		}
	})
}

func TestValidateBytesAllowlistBlacklistAndAutoLimit(t *testing.T) {
	t.Run("invalid entries and negative limit are errors", func(t *testing.T) {
		raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["", "*", "*@", "*@*", "user*@example.com", "Name <a@example.com>", "user@localhost"]
send_blacklist = ["*@company.example", "not-an-email"]
daily_auto_limit = -5
`
		report := ValidateBytes("config.toml", []byte(raw))
		if report.Valid {
			t.Fatal("invalid allowlist/blacklist/limit must fail")
		}
		invalidAllow := 0
		for _, d := range report.Diagnostics {
			if d.Code == "invalid_allowlist_entry" && d.Severity == SeverityError {
				invalidAllow++
			}
		}
		if invalidAllow != 7 {
			t.Errorf("invalid_allowlist_entry count = %d, want 7 (%+v)", invalidAllow, report.Diagnostics)
		}
		if !hasDiagCode(report, SeverityError, "blacklist_wildcard_unsupported") {
			t.Errorf("expected blacklist_wildcard_unsupported in %+v", report.Diagnostics)
		}
		if !hasDiagCode(report, SeverityError, "invalid_blacklist_entry") {
			t.Errorf("expected invalid_blacklist_entry in %+v", report.Diagnostics)
		}
		if !hasDiagCode(report, SeverityError, "invalid_daily_auto_limit") {
			t.Errorf("expected invalid_daily_auto_limit in %+v", report.Diagnostics)
		}
	})

	t.Run("broad wildcards, redundant blacklist, zero limit, and empty autosend allowlist warn", func(t *testing.T) {
		raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["*@qq.com", "*@gmail.com"]
send_blacklist = ["nobody@other.example"]
auto_send = true
daily_auto_limit = 0

[accounts.empty_auto]
email = "other@qq.com"
auto_send = true
`
		report := ValidateBytes("config.toml", []byte(raw))
		if !report.Valid {
			t.Fatalf("warnings-only config should be valid: %+v", report.Diagnostics)
		}
		for _, code := range []string{
			"broad_allowlist_wildcard",
			"redundant_blacklist_entry",
			"daily_auto_limit_normalized",
			"autosend_empty_allowlist",
		} {
			if !hasDiagCode(report, SeverityWarning, code) {
				t.Errorf("expected warning %q in %+v", code, report.Diagnostics)
			}
		}
	})
}

func TestBuildPolicyPreview(t *testing.T) {
	raw := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
smtp_port = 587
send_allowlist = ["reader@example.com"]
auto_send = true
send_blaklist = ["boss@example.com"]
`
	path := writeTempConfig(t, "config.toml", raw)
	cfg, loadedPath, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := BuildPolicyPreview(cfg, loadedPath, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Readonly || len(preview.Accounts) != 1 || preview.PolicyBoundaryNote == "" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	acct := preview.Accounts[0]
	if acct.CustomIMAP || !acct.CustomSMTP || acct.IMAPHost != DefaultIMAPHost || acct.SMTPPort != 587 {
		t.Fatalf("unexpected endpoints: %+v", acct)
	}
	if !acct.AllowlistReady || !acct.AutoSend || acct.AutoSendEffective || acct.DailyAutoLimit != DefaultDailyAutoLimit || !acct.DailyAutoLimitDefault {
		t.Fatalf("unexpected autosend policy: %+v", acct)
	}
	if !slices.Contains(acct.UnknownKeys, "accounts.personal.send_blaklist") {
		t.Fatalf("unknown_keys missing: %+v", acct.UnknownKeys)
	}
	if !slices.Contains(acct.AutoSendBlockReasons, "readonly") || !slices.Contains(acct.AutoSendBlockReasons, "unknown_keys") {
		t.Fatalf("expected readonly and unknown_keys block reasons, got %+v", acct.AutoSendBlockReasons)
	}
}

func TestDiffFilesClassification(t *testing.T) {
	base := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["alice@company.example", "bob@company.example"]
auto_send = false
send_blacklist = ["alice@company.example"]
daily_auto_limit = 20
`
	t.Run("expands permissions across multiple dimensions", func(t *testing.T) {
		proposed := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
smtp_host = "smtp.custom.example"
send_allowlist = ["alice@company.example", "bob@company.example", "*@company.example"]
auto_send = true
send_blacklist = []
daily_auto_limit = 40

[accounts.bot]
email = "bot@qq.com"
send_allowlist = ["ops@company.example"]
`
		diff, err := DiffFiles(writeTempConfig(t, "cur.toml", base), writeTempConfig(t, "prop.toml", proposed), "")
		if err != nil {
			t.Fatal(err)
		}
		if !diff.HasChanges || !diff.ExpandsPermissions {
			t.Fatalf("expected expanding diff: %+v", diff)
		}
		wantFields := map[string]bool{
			"smtp_endpoint":    false,
			"send_allowlist":   false,
			"auto_send":        false,
			"daily_auto_limit": false,
			"send_blacklist":   false,
			"account":          false,
		}
		for _, ch := range diff.Changes {
			if ch.Impact == ImpactExpands {
				wantFields[ch.Field] = true
			}
		}
		for field, seen := range wantFields {
			if !seen {
				t.Errorf("expected expands change on field %q in %+v", field, diff.Changes)
			}
		}
	})

	t.Run("restricts permissions does not flag expands_permissions", func(t *testing.T) {
		cur := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
imap_host = "imap.custom.example"
send_allowlist = ["*@company.example"]
auto_send = true
send_blacklist = []
daily_auto_limit = 50
`
		prop := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["alice@company.example"]
auto_send = false
send_blacklist = ["alice@company.example"]
daily_auto_limit = 10
`
		diff, err := DiffFiles(writeTempConfig(t, "cur.toml", cur), writeTempConfig(t, "prop.toml", prop), "")
		if err != nil {
			t.Fatal(err)
		}
		if !diff.HasChanges || diff.ExpandsPermissions || diff.Summary.Restricts == 0 {
			t.Fatalf("narrowing diff must not flag expands_permissions: %+v", diff)
		}
	})

	t.Run("equivalent defaults produce no diff", func(t *testing.T) {
		cur := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
`
		prop := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
imap_host = "imap.qq.com"
imap_port = 993
smtp_host = "smtp.qq.com"
smtp_port = 465
daily_auto_limit = 50
`
		diff, err := DiffFiles(writeTempConfig(t, "cur.toml", cur), writeTempConfig(t, "prop.toml", prop), "")
		if err != nil {
			t.Fatal(err)
		}
		if diff.HasChanges || diff.ExpandsPermissions || diff.Summary.Total != 0 {
			t.Fatalf("equivalent effective policy should have zero changes: %+v", diff)
		}
	})

	t.Run("switching default_account to broader or less-blacklisted account expands permissions", func(t *testing.T) {
		cur := `
schema = 1
default_account = "strict"

[accounts.strict]
email = "strict@qq.com"
send_allowlist = ["alice@company.example"]
auto_send = true
send_blacklist = ["alice@company.example"]

[accounts.open]
email = "open@qq.com"
send_allowlist = ["alice@company.example"]
auto_send = true
`
		prop := strings.Replace(cur, `default_account = "strict"`, `default_account = "open"`, 1)
		diff, err := DiffFiles(writeTempConfig(t, "cur.toml", cur), writeTempConfig(t, "prop.toml", prop), "")
		if err != nil {
			t.Fatal(err)
		}
		if !diff.HasChanges || !diff.ExpandsPermissions {
			t.Fatalf("switching default_account to less-restricted account must expand permissions: %+v", diff)
		}
	})

	t.Run("invalid proposed config fails with errmap.Config and diagnostics context", func(t *testing.T) {
		badProposed := `
schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_blaklist = ["boss@company.example"]
`
		_, err := DiffFiles(writeTempConfig(t, "cur.toml", base), writeTempConfig(t, "bad.toml", badProposed), "")
		if err == nil || errmap.Classify(err).Kind != errmap.Config {
			t.Fatalf("expected errmap.Config error, got %v", err)
		}
		ctx := errmap.Classify(err).Context
		propVal, ok := ctx["proposed_validation"].(ValidationReport)
		if !ok || propVal.Valid || !hasDiagCode(propVal, SeverityError, "unknown_field") {
			t.Fatalf("expected proposed_validation with unknown_field in error context: %#v", ctx)
		}
	})
}

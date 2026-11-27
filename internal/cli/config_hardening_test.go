package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/policy"
)

func TestConfigRewritePathsRefuseToDropUnknownKeys(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")

	cases := []struct {
		name      string
		rawConfig string
		wantKey   string
	}{
		{
			name: "misspelled blacklist keeps autosend fail-closed across rewrite attempts",
			rawConfig: `schema = 1
default_account = "personal"

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["blocked@example.com"]
auto_send = true
send_blaklist = ["blocked@example.com"]

[accounts.work]
email = "work@foxmail.com"
`,
			wantKey: "accounts.personal.send_blaklist",
		},
		{
			name: "top-level unknown key blocks all config rewrite paths",
			rawConfig: `schema = 1
default_account = "personal"
unknown_top_policy = true

[accounts.personal]
email = "user@qq.com"
send_allowlist = ["blocked@example.com"]
auto_send = true

[accounts.work]
email = "work@foxmail.com"
`,
			wantKey: "unknown_top_policy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.rawConfig), 0o600); err != nil {
				t.Fatal(err)
			}

			// 1. account use must refuse and leave file byte-for-byte untouched.
			rtUse := &Runtime{Out: &bytes.Buffer{}, Err: io.Discard}
			rootUse := NewRoot(rtUse)
			rootUse.SetArgs([]string{"--config", path, "--json", "account", "use", "work"})
			err := rootUse.Execute()
			if err == nil {
				t.Fatal("account use must refuse rewriting config with unknown keys")
			}
			detail, code := errmap.Details(err)
			if code != output.ExitConfig || !containsStringAny(detail.Context["unknown_keys"], tc.wantKey) {
				t.Fatalf("account use: got code=%d detail=%+v", code, detail)
			}

			// 2. auth login (re-login) must refuse BEFORE dialing IMAP or writing keyring.
			provider := &stubKeyringProvider{values: map[string]string{"user@qq.com": "existingAuth1234", "work@foxmail.com": "workAuthCode1234"}}
			rtLogin := &Runtime{
				Out:     &bytes.Buffer{},
				Err:     io.Discard,
				In:      strings.NewReader("newAuthCode56789\n"),
				Secrets: provider,
				Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
					t.Fatal("auth login must fail before dialing IMAP when config has unknown keys")
					return nil, nil
				},
			}
			rootLogin := NewRoot(rtLogin)
			rootLogin.SetArgs([]string{"--config", path, "--json", "auth", "login", "--email", "user@qq.com", "--name", "personal", "--auth-code-stdin"})
			err = rootLogin.Execute()
			if err == nil {
				t.Fatal("auth login must refuse rewriting config with unknown keys")
			}
			detail, code = errmap.Details(err)
			if code != output.ExitConfig || !containsStringAny(detail.Context["unknown_keys"], tc.wantKey) {
				t.Fatalf("auth login: got code=%d detail=%+v", code, detail)
			}
			if provider.setCalls != 0 || provider.deleteCalls != 0 {
				t.Fatalf("auth login touched keyring despite unwritable config: set=%d delete=%d", provider.setCalls, provider.deleteCalls)
			}

			// 3. auth logout must refuse BEFORE deleting stored credentials.
			rtLogout := &Runtime{Out: &bytes.Buffer{}, Err: io.Discard, Secrets: provider}
			rootLogout := NewRoot(rtLogout)
			rootLogout.SetArgs([]string{"--config", path, "--json", "auth", "logout", "--name", "work"})
			err = rootLogout.Execute()
			if err == nil {
				t.Fatal("auth logout must refuse rewriting config with unknown keys")
			}
			detail, code = errmap.Details(err)
			if code != output.ExitConfig || !containsStringAny(detail.Context["unknown_keys"], tc.wantKey) {
				t.Fatalf("auth logout: got code=%d detail=%+v", code, detail)
			}
			if provider.deleteCalls != 0 || provider.values["work@foxmail.com"] != "workAuthCode1234" {
				t.Fatalf("auth logout deleted credential despite unwritable config")
			}

			// 4. Config file remains byte-for-byte identical, and autosend still fails closed.
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, []byte(tc.rawConfig)) {
				t.Fatalf("config file changed after refused rewrite commands")
			}
			cfg, _, loadErr := account.Load(path)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			named, resolveErr := cfg.Resolve("personal")
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			eligible, autoErr := autoSendEligible(named, []string{"blocked@example.com"}, time.Now())
			if eligible || autoErr == nil {
				t.Fatalf("autosend fail-closed protection was bypassed: eligible=%v err=%v", eligible, autoErr)
			}
			if _, autoCode := errmap.Details(autoErr); autoCode != output.ExitPolicyDenied {
				t.Fatalf("expected exit 50 policy_denied, got %d (%v)", autoCode, autoErr)
			}
		})
	}
}

func containsStringAny(value any, want string) bool {
	list, ok := value.([]string)
	if !ok {
		return false
	}
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

func TestAuthLoginDefaultDoesNotBlockReadonlyLoginOnSMTP(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	provider := &secrets.Memory{Values: map[string]string{}}
	var out bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: io.Discard, In: strings.NewReader("abcdefghijklmnop\n"),
		Secrets: provider,
		Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
		VerifySMTP: func(context.Context, account.Named, string) (sendmail.VerifyResult, error) {
			t.Fatal("default auth login must not call VerifySMTP without --verify-smtp")
			return sendmail.VerifyResult{}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "auth", "login", "--email", "user@qq.com", "--auth-code-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("auth login failed: %v", err)
	}
	validateOutput(t, "auth.login.schema.json", out.Bytes())
	var doc struct {
		OK   bool `json:"ok"`
		Data struct {
			Verified     bool   `json:"verified"`
			IMAPVerified bool   `json:"imap_verified"`
			SMTPChecked  bool   `json:"smtp_checked"`
			SMTPVerified bool   `json:"smtp_verified"`
			SMTPServer   string `json:"smtp_server"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || !doc.Data.Verified || !doc.Data.IMAPVerified || doc.Data.SMTPChecked || doc.Data.SMTPVerified || doc.Data.SMTPServer != "smtp.qq.com:465" {
		t.Fatalf("unexpected auth login output: %+v", doc.Data)
	}
}

func TestAuthLoginVerifySMTPSuccessAndFailure(t *testing.T) {
	t.Run("success reports independent IMAP and SMTP status", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.toml")
		provider := &secrets.Memory{Values: map[string]string{}}
		var out bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: io.Discard, In: strings.NewReader("abcdefghijklmnop\n"),
			Secrets: provider,
			Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
			VerifySMTP: func(context.Context, account.Named, string) (sendmail.VerifyResult, error) {
				return sendmail.VerifyResult{
					Server:       "smtp.qq.com:587",
					Endpoint:     "127.0.0.1:587",
					Transport:    "starttls",
					FallbackUsed: true,
					Attempts:     1,
					Extensions:   []string{"8BITMIME", "AUTH"},
				}, nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "auth", "login", "--email", "user@qq.com", "--auth-code-stdin", "--verify-smtp"})
		if err := root.Execute(); err != nil {
			t.Fatalf("auth login --verify-smtp failed: %v", err)
		}
		validateOutput(t, "auth.login.schema.json", out.Bytes())
		var doc struct {
			Data struct {
				IMAPVerified     bool     `json:"imap_verified"`
				SMTPChecked      bool     `json:"smtp_checked"`
				SMTPVerified     bool     `json:"smtp_verified"`
				SMTPTransport    string   `json:"smtp_transport"`
				SMTPFallbackUsed bool     `json:"smtp_fallback_used"`
				SMTPExtensions   []string `json:"smtp_extensions"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if !doc.Data.IMAPVerified || !doc.Data.SMTPChecked || !doc.Data.SMTPVerified || doc.Data.SMTPTransport != "starttls" || !doc.Data.SMTPFallbackUsed {
			t.Fatalf("unexpected verify-smtp output: %+v", doc.Data)
		}
		if provider.Values["user@qq.com"] != "abcdefghijklmnop" {
			t.Fatal("credential was not saved after successful IMAP+SMTP verification")
		}
	})

	t.Run("SMTP failure does not save credential or leak secret", func(t *testing.T) {
		const secret = "abcdefghijklmnop"
		configPath := filepath.Join(t.TempDir(), "config.toml")
		provider := &secrets.Memory{Values: map[string]string{}}
		var out bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: io.Discard, In: strings.NewReader(secret + "\n"),
			Secrets: provider,
			Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
			VerifySMTP: func(context.Context, account.Named, string) (sendmail.VerifyResult, error) {
				return sendmail.VerifyResult{}, &errmap.Error{
					Kind:    errmap.AuthFailed,
					Message: "SMTP 认证失败",
					Context: map[string]any{"stage": sendmail.StageAuth, "server": "smtp.qq.com:465"},
				}
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "auth", "login", "--email", "user@qq.com", "--auth-code-stdin", "--verify-smtp"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected SMTP verification failure")
		}
		failure, code := errmap.Details(err)
		if code != output.ExitAuth || failure.Context["imap_verified"] != true || failure.Context["smtp_verified"] != false {
			t.Fatalf("unexpected failure details: code=%d failure=%+v", code, failure)
		}
		if err := output.WriteJSON(&out, output.Failure(rt.current, failure, time.Now())); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.login.schema.json", out.Bytes())
		if bytes.Contains(out.Bytes(), []byte(secret)) {
			t.Fatal("secret leaked into auth login failure output")
		}
		if _, exists := provider.Values["user@qq.com"]; exists {
			t.Fatal("credential must not be persisted when --verify-smtp fails")
		}
		if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
			t.Fatalf("config file must not be written when --verify-smtp fails: %v", statErr)
		}
	})

	t.Run("readonly blocks auth login --verify-smtp before dialing", func(t *testing.T) {
		t.Setenv("QQMAIL_CLI_READONLY", "1")
		configPath := filepath.Join(t.TempDir(), "config.toml")
		rt := &Runtime{
			Out: &bytes.Buffer{}, Err: io.Discard, In: strings.NewReader("abcdefghijklmnop\n"),
			Secrets: &secrets.Memory{Values: map[string]string{}},
			Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
				t.Fatal("readonly must block IMAP dial")
				return nil, nil
			},
			VerifySMTP: func(context.Context, account.Named, string) (sendmail.VerifyResult, error) {
				t.Fatal("readonly must block SMTP verification")
				return sendmail.VerifyResult{}, nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "auth", "login", "--email", "user@qq.com", "--auth-code-stdin", "--verify-smtp"})
		err := root.Execute()
		_, code := errmap.Details(err)
		if code != output.ExitPolicyDenied {
			t.Fatalf("expected exit 50 under readonly, got %d (%v)", code, err)
		}
	})
}

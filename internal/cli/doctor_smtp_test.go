package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

func TestDoctorSMTPAuthenticatedSuccessAndSchema(t *testing.T) {
	configPath := saveSendConfig(t, nil)
	const fixtureSecret = "synthetic-secret-16"
	provider := &secrets.Memory{Values: map[string]string{"user@qq.com": fixtureSecret}}
	var out bytes.Buffer
	var smtpCalled bool
	rt := &Runtime{
		Out: &out, Err: io.Discard, Secrets: provider,
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
		ProbeSMTP: func(_ context.Context, _ account.Named, code string, opts sendmail.ProbeOptions) (sendmail.ProbeResult, error) {
			smtpCalled = true
			if opts.NoAuth || code != fixtureSecret {
				t.Fatalf("expected authenticated SMTP probe with secret, got noAuth=%v", opts.NoAuth)
			}
			return sendmail.ProbeResult{
				AuthMode:     "authenticated",
				Server:       "smtp.qq.com:587",
				Endpoint:     "127.0.0.1:587",
				Transport:    "starttls",
				FallbackUsed: true,
				DurationMS:   12,
				Extensions:   []string{"8BITMIME", "AUTH"},
				Stages: []netx.StageReport{
					{Stage: sendmail.StageDNS, Status: netx.StageStatusPass, DurationMS: 2, Transport: "starttls"},
					{Stage: sendmail.StageTCP, Status: netx.StageStatusPass, DurationMS: 3, Endpoint: "127.0.0.1:587", Transport: "starttls"},
					{Stage: sendmail.StageSTARTTLS, Status: netx.StageStatusPass, DurationMS: 2, Endpoint: "127.0.0.1:587", Transport: "starttls"},
					{Stage: sendmail.StageTLS, Status: netx.StageStatusPass, DurationMS: 3, Endpoint: "127.0.0.1:587", Transport: "starttls"},
					{Stage: sendmail.StageAuth, Status: netx.StageStatusPass, DurationMS: 2, Endpoint: "127.0.0.1:587", Transport: "starttls"},
				},
			}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "doctor", "--smtp"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor --smtp failed: %v", err)
	}
	if !smtpCalled {
		t.Fatal("expected ProbeSMTP to be invoked")
	}
	validateOutput(t, "doctor.schema.json", out.Bytes())
	if bytes.Contains(out.Bytes(), []byte(fixtureSecret)) {
		t.Fatal("doctor --smtp leaked credential")
	}
	var doc struct {
		OK   bool `json:"ok"`
		Data struct {
			AuthMode string `json:"auth_mode"`
			IMAP     struct {
				Checked bool `json:"checked"`
				OK      bool `json:"ok"`
			} `json:"imap"`
			SMTP struct {
				Checked      bool               `json:"checked"`
				OK           bool               `json:"ok"`
				AuthMode     string             `json:"auth_mode"`
				Transport    string             `json:"transport"`
				FallbackUsed bool               `json:"fallback_used"`
				Stages       []netx.StageReport `json:"stages"`
			} `json:"smtp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.Data.AuthMode != "authenticated" || !doc.Data.IMAP.Checked || !doc.Data.SMTP.Checked || !doc.Data.SMTP.OK || !doc.Data.SMTP.FallbackUsed || len(doc.Data.SMTP.Stages) != 5 {
		t.Fatalf("unexpected doctor --smtp output: %+v", doc.Data)
	}
}

func TestDoctorSMTPOnlyUnauthenticatedSkipsConfigCredentialAndIMAP(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "missing.toml")
	var out bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: io.Discard,
		Secrets: &secrets.Memory{Values: map[string]string{}},
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			t.Fatal("doctor --smtp-only must not dial IMAP")
			return nil, nil
		},
		ProbeIMAP: func(context.Context, account.Named, string, imapx.ProbeOptions) (imapx.ProbeResult, error) {
			t.Fatal("doctor --smtp-only must not probe IMAP")
			return imapx.ProbeResult{}, nil
		},
		ProbeSMTP: func(_ context.Context, named account.Named, code string, opts sendmail.ProbeOptions) (sendmail.ProbeResult, error) {
			if !opts.NoAuth || code != "" {
				t.Fatalf("expected unauthenticated probe with empty secret, got noAuth=%v code=%q", opts.NoAuth, code)
			}
			if named.SMTPHost != "smtp.qq.com" || named.SMTPPort != 465 {
				t.Fatalf("expected default QQ SMTP endpoint when unconfigured, got %s:%d", named.SMTPHost, named.SMTPPort)
			}
			return sendmail.ProbeResult{
				AuthMode:   "unauthenticated",
				Server:     "smtp.qq.com:465",
				Endpoint:   "127.0.0.1:465",
				Transport:  "implicit_tls",
				DurationMS: 8,
				Extensions: []string{"AUTH", "SIZE"},
				Stages: []netx.StageReport{
					{Stage: sendmail.StageDNS, Status: netx.StageStatusPass, DurationMS: 1, Transport: "implicit_tls"},
					{Stage: sendmail.StageTCP, Status: netx.StageStatusPass, DurationMS: 2, Endpoint: "127.0.0.1:465", Transport: "implicit_tls"},
					{Stage: sendmail.StageTLS, Status: netx.StageStatusPass, DurationMS: 3, Endpoint: "127.0.0.1:465", Transport: "implicit_tls"},
					{Stage: sendmail.StageGreeting, Status: netx.StageStatusPass, DurationMS: 1, Endpoint: "127.0.0.1:465", Transport: "implicit_tls"},
					{Stage: sendmail.StageEHLO, Status: netx.StageStatusPass, DurationMS: 1, Endpoint: "127.0.0.1:465", Transport: "implicit_tls"},
				},
			}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", missingConfig, "--json", "doctor", "--smtp-only", "--no-auth"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor --smtp-only --no-auth failed: %v", err)
	}
	validateOutput(t, "doctor.schema.json", out.Bytes())
	var doc struct {
		OK   bool `json:"ok"`
		Data struct {
			AuthMode string `json:"auth_mode"`
			IMAP     struct {
				Checked bool `json:"checked"`
			} `json:"imap"`
			SMTP struct {
				Checked  bool   `json:"checked"`
				OK       bool   `json:"ok"`
				AuthMode string `json:"auth_mode"`
			} `json:"smtp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.Data.AuthMode != "unauthenticated" || doc.Data.IMAP.Checked || !doc.Data.SMTP.Checked || !doc.Data.SMTP.OK || doc.Data.SMTP.AuthMode != "unauthenticated" {
		t.Fatalf("unexpected --smtp-only --no-auth output: %+v", doc.Data)
	}
}

func TestDoctorSMTPFailureReportsStageCompletedChecksAndNoSecret(t *testing.T) {
	configPath := saveSendConfig(t, nil)
	const fixtureSecret = "synthetic-secret-16"
	provider := &secrets.Memory{Values: map[string]string{"user@qq.com": fixtureSecret}}
	var out bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: io.Discard, Secrets: provider,
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
		ProbeSMTP: func(context.Context, account.Named, string, sendmail.ProbeOptions) (sendmail.ProbeResult, error) {
			return sendmail.ProbeResult{
					AuthMode:  "authenticated",
					Server:    "smtp.qq.com:465",
					Endpoint:  "127.0.0.1:465",
					Transport: "implicit_tls",
					Stages: []netx.StageReport{
						{Stage: sendmail.StageDNS, Status: netx.StageStatusPass, DurationMS: 1},
						{Stage: sendmail.StageTCP, Status: netx.StageStatusPass, DurationMS: 2, Endpoint: "127.0.0.1:465"},
						{Stage: sendmail.StageTLS, Status: netx.StageStatusFail, DurationMS: 5, Endpoint: "127.0.0.1:465", ErrorCategory: netx.CategoryTLSCertificate},
					},
				}, (&errmap.Error{
					Kind:    errmap.TLS,
					Message: "TLS 证书校验失败",
					Context: map[string]any{
						"stage":          sendmail.StageTLS,
						"server":         "smtp.qq.com:465",
						"endpoint":       "127.0.0.1:465",
						"transport":      "implicit_tls",
						"error_category": netx.CategoryTLSCertificate,
					},
				}).WithRetryable(false)
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "doctor", "--smtp"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected doctor --smtp failure")
	}
	failure, exitCode := errmap.Details(err)
	if exitCode != output.ExitTLS || failure.Retryable {
		t.Fatalf("expected non-retryable exit 21, got exit=%d failure=%+v", exitCode, failure)
	}
	if err := output.WriteJSON(&out, output.Failure(rt.current, failure, time.Now())); err != nil {
		t.Fatal(err)
	}
	validateOutput(t, "doctor.schema.json", out.Bytes())
	if bytes.Contains(out.Bytes(), []byte(fixtureSecret)) {
		t.Fatal("doctor --smtp failure leaked credential")
	}
	var doc struct {
		Error struct {
			Context struct {
				Stage     string             `json:"stage"`
				Protocol  string             `json:"protocol"`
				AuthMode  string             `json:"auth_mode"`
				Completed []string           `json:"completed_checks"`
				Stages    []netx.StageReport `json:"stages"`
			} `json:"context"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error.Context.Stage != sendmail.StageTLS || doc.Error.Context.Protocol != "smtp" || doc.Error.Context.AuthMode != "authenticated" || len(doc.Error.Context.Stages) != 3 {
		t.Fatalf("unexpected failure context: %+v", doc.Error.Context)
	}
}

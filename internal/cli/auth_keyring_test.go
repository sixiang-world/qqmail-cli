package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/situker/qqmail-cli/internal/secrets"
)

type stubKeyringProvider struct {
	values      map[string]string
	setErr      error
	getErr      error
	deleteErr   error
	source      string
	setCalls    int
	getCalls    int
	deleteCalls int
	lastSetVal  string
}

func (s *stubKeyringProvider) Source() string {
	if s.source != "" {
		return s.source
	}
	return "keyring"
}

func (s *stubKeyringProvider) Get(acct string) (string, error) {
	s.getCalls++
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[acct]
	if !ok {
		return "", &errmap.Error{Kind: errmap.AuthCodeMissing, Message: "missing"}
	}
	output.RegisterSecret(v)
	return v, nil
}

func (s *stubKeyringProvider) Set(acct, code string) error {
	s.setCalls++
	s.lastSetVal = code
	output.RegisterSecret(code)
	if s.setErr != nil {
		return s.setErr
	}
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[acct] = code
	return nil
}

func (s *stubKeyringProvider) Delete(acct string) error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, acct)
	return nil
}

func fakeLinuxKeyringErr(reason secrets.Reason, dbusSet bool, observed string) error {
	return &errmap.Error{
		Kind:       errmap.Config,
		Message:    "无法写入系统凭据管理器",
		Suggestion: secrets.SuggestionFor("linux", reason),
		Context: map[string]any{
			"credential_store":     "keyring",
			"reason":               string(reason),
			"platform":             "linux",
			"dbus_session_bus_set": dbusSet,
			"observed":             observed,
		},
		Cause: errors.New(observed),
	}
}

func TestAuthLoginDistinguishesIMAPVerifiedFromCredentialSaveFailure(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")
	const syntheticCode = "zx98cv76bn54mk32"
	encodedCode := base64.StdEncoding.EncodeToString([]byte(syntheticCode))

	cases := []struct {
		name     string
		reason   secrets.Reason
		dbusSet  bool
		observed string
	}{
		{"no_session_bus", secrets.ReasonNoSessionBus, false, `exec: "dbus-launch": executable file not found in $PATH`},
		{"service_unavailable", secrets.ReasonServiceUnavailable, true, "The name org.freedesktop.secrets was not provided by any .service files"},
		{"locked", secrets.ReasonLocked, true, "failed to unlock correct collection '/org/freedesktop/secrets/collection/login'"},
		{"permission_denied", secrets.ReasonPermissionDenied, true, "org.freedesktop.DBus.Error.AccessDenied"},
		{"timeout", secrets.ReasonTimeout, true, "credential store operation timed out after 15s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.toml")
			provider := &stubKeyringProvider{
				setErr: fakeLinuxKeyringErr(tc.reason, tc.dbusSet, tc.observed),
			}
			var out, stderr bytes.Buffer
			rt := &Runtime{
				Build:   BuildInfo{Version: "test"},
				Out:     &out,
				Err:     &stderr,
				In:      strings.NewReader(syntheticCode + "\n"),
				Secrets: provider,
				Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
			}
			root := NewRoot(rt)
			root.SetArgs([]string{"--config", configPath, "--json", "auth", "login", "--email", "user@qq.com", "--name", "personal", "--auth-code-stdin"})
			err := root.Execute()
			if err == nil {
				t.Fatal("expected auth login to fail when credential save fails")
			}

			// Config file must NOT be written when credential save fails.
			if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
				t.Fatalf("config file was unexpectedly created on credential save failure: %v", statErr)
			}

			failure, code := errmap.Details(err)
			if failure.Code != string(errmap.Config) || code != output.ExitConfig || failure.Retryable {
				t.Fatalf("unexpected error semantics: exit=%d failure=%+v", code, failure)
			}
			if !strings.Contains(failure.Message, "IMAP") || !strings.Contains(failure.Message, "登录尚未完成") || !strings.Contains(failure.Message, "配置未写入") {
				t.Fatalf("message must distinguish IMAP pass from incomplete login/save failure: %q", failure.Message)
			}
			if !strings.Contains(failure.Suggestion, "auth login") {
				t.Fatalf("suggestion should tell user to re-run auth login after fixing store: %q", failure.Suggestion)
			}

			// Validate JSON failure envelope against auth.login.schema.json.
			var envBuf bytes.Buffer
			if writeErr := output.WriteJSON(&envBuf, output.Failure(rt.current, failure, time.Now())); writeErr != nil {
				t.Fatal(writeErr)
			}
			validateOutput(t, "auth.login.schema.json", envBuf.Bytes())

			// Check structured context fields: definite failures assert
			// credential_saved=false, whereas timeout marks outcome_uncertain=true
			// without falsely claiming the uncancelable OS write did not happen.
			ctx := failure.Context
			if ctx["stage"] != "credential_store" || ctx["imap_verified"] != true || ctx["config_saved"] != false {
				t.Fatalf("missing/wrong auth login failure context flags: %+v", ctx)
			}
			if tc.reason == secrets.ReasonTimeout {
				if ctx["outcome_uncertain"] != true || ctx["credential_save_state"] != secrets.OutcomeUncertain {
					t.Fatalf("timeout must mark outcome_uncertain=true and credential_save_state=uncertain: %+v", ctx)
				}
				if _, hasFalse := ctx["credential_saved"]; hasFalse {
					t.Fatalf("timeout must not assert definite credential_saved=false: %+v", ctx)
				}
				if !strings.Contains(failure.Suggestion, "auth status") {
					t.Fatalf("timeout suggestion should direct status confirmation: %q", failure.Suggestion)
				}
			} else {
				if ctx["credential_saved"] != false || ctx["credential_save_state"] != secrets.OutcomeFailed || ctx["outcome_uncertain"] != false {
					t.Fatalf("definite failure context wrong: %+v", ctx)
				}
			}
			if ctx["credential_store"] != "keyring" || ctx["reason"] != string(tc.reason) || ctx["platform"] != "linux" || ctx["dbus_session_bus_set"] != tc.dbusSet {
				t.Fatalf("missing/wrong credential store context: %+v", ctx)
			}

			// Verify zero secret leakage across all channels.
			combined := out.String() + "\n" + stderr.String() + "\n" + envBuf.String() + "\n" + err.Error()
			if strings.Contains(combined, syntheticCode) || strings.Contains(combined, encodedCode) {
				t.Fatalf("synthetic auth code leaked into outputs")
			}
		})
	}
}

func TestAuthLoginFailureLeavesExistingConfigUntouched(t *testing.T) {
	t.Setenv(policy.ReadonlyEnv, "0")
	configPath := filepath.Join(t.TempDir(), "config.toml")
	initial := &account.Config{
		Schema:         account.ConfigSchema,
		DefaultAccount: "existing",
		Accounts: map[string]account.Account{
			"existing": {Email: "existing@qq.com", SendAllowlist: []string{"safe@example.com"}},
		},
	}
	if err := initial.Save(configPath); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{
		Build:   BuildInfo{Version: "test"},
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
		In:      strings.NewReader("qw12er34ty56ui78\n"),
		Secrets: &stubKeyringProvider{setErr: fakeLinuxKeyringErr(secrets.ReasonLocked, true, "collection is locked")},
		Dial:    func(context.Context, account.Named, string) (imapx.Reader, error) { return fakeReader{}, nil },
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "auth", "login", "--email", "new@qq.com", "--name", "second", "--auth-code-stdin"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when credential save fails")
	}

	afterBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("existing config was modified despite credential save failure\nbefore:\n%s\nafter:\n%s", beforeBytes, afterBytes)
	}
}

func TestAuthKeyringTestCommand(t *testing.T) {
	t.Run("success round trip matches schema and protects secrets", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{values: map[string]string{"user@qq.com": "realAuthCode1234"}}
		dialed := false
		var out, stderr bytes.Buffer
		rt := &Runtime{
			Out:     &out,
			Err:     &stderr,
			In:      strings.NewReader(""),
			Secrets: provider,
			Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
				dialed = true
				return fakeReader{}, nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		if err := root.Execute(); err != nil {
			t.Fatalf("auth keyring-test failed: %v (stderr=%s)", err, stderr.String())
		}
		if dialed {
			t.Fatal("auth keyring-test must never dial mail server")
		}
		validateOutput(t, "auth.keyring-test.schema.json", out.Bytes())
		if provider.lastSetVal == "" {
			t.Fatal("probe never wrote a value")
		}
		if len(provider.values) != 1 || provider.values["user@qq.com"] != "realAuthCode1234" {
			t.Fatalf("probe left unexpected entries or altered real credential: %+v", provider.values)
		}
		if strings.Contains(out.String(), provider.lastSetVal) || strings.Contains(out.String(), "realAuthCode1234") {
			t.Fatalf("secret or probe value leaked in JSON output: %s", out.String())
		}
	})

	t.Run("human text output succeeds and is sanitized", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{}
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"auth", "keyring-test"})
		if err := root.Execute(); err != nil {
			t.Fatalf("human auth keyring-test failed: %v", err)
		}
		if !strings.Contains(out.String(), "cleanup_ok=true") || strings.Contains(out.String(), provider.lastSetVal) {
			t.Fatalf("unexpected human output: %q", out.String())
		}
	})

	t.Run("set failure returns config error matching schema", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{
			setErr: fakeLinuxKeyringErr(secrets.ReasonServiceUnavailable, true, "The name org.freedesktop.secrets was not provided by any .service files"),
		}
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected failure")
		}
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.Config) || code != output.ExitConfig {
			t.Fatalf("got code=%s exit=%d", failure.Code, code)
		}
		if failure.Context["failed_step"] != "set" || failure.Context["reason"] != string(secrets.ReasonServiceUnavailable) {
			t.Fatalf("unexpected context: %+v", failure.Context)
		}
		var envBuf bytes.Buffer
		if err := output.WriteJSON(&envBuf, output.Failure(rt.current, failure, time.Now())); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.keyring-test.schema.json", envBuf.Bytes())
		if provider.lastSetVal != "" && strings.Contains(envBuf.String(), provider.lastSetVal) {
			t.Fatal("probe value leaked into failure output")
		}
	})

	t.Run("get failure still performs delete cleanup", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{
			getErr: fakeLinuxKeyringErr(secrets.ReasonLocked, true, "collection is locked"),
		}
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected failure")
		}
		failure, code := errmap.Details(err)
		if code != output.ExitConfig || failure.Context["failed_step"] != "get" || failure.Context["cleanup_ok"] != true || failure.Context["cleanup_attempted"] != true {
			t.Fatalf("unexpected failure context: code=%d ctx=%+v", code, failure.Context)
		}
		if provider.deleteCalls != 1 {
			t.Fatalf("expected cleanup delete call, got %d", provider.deleteCalls)
		}
		if len(provider.values) != 0 {
			t.Fatalf("probe value remained after cleanup: %+v", provider.values)
		}
		var envBuf bytes.Buffer
		if err := output.WriteJSON(&envBuf, output.Failure(rt.current, failure, time.Now())); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.keyring-test.schema.json", envBuf.Bytes())
	})

	t.Run("delete failure reports cleanup_ok false", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{
			deleteErr: fakeLinuxKeyringErr(secrets.ReasonPermissionDenied, true, "org.freedesktop.DBus.Error.AccessDenied"),
		}
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected failure")
		}
		failure, code := errmap.Details(err)
		if code != output.ExitConfig || failure.Context["failed_step"] != "delete" || failure.Context["cleanup_ok"] != false || failure.Context["cleanup_status"] != secrets.CleanupStatusFailed {
			t.Fatalf("unexpected failure context: code=%d ctx=%+v", code, failure.Context)
		}
		probeAcct, _ := failure.Context["probe_account"].(string)
		if !strings.HasPrefix(probeAcct, secrets.ProbeAccount+"-") || !strings.Contains(failure.Message, probeAcct) {
			t.Fatalf("expected cleanup failure message to identify unique probe account %q: %q", probeAcct, failure.Message)
		}
		var envBuf bytes.Buffer
		if err := output.WriteJSON(&envBuf, output.Failure(rt.current, failure, time.Now())); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.keyring-test.schema.json", envBuf.Bytes())
	})

	t.Run("set timeout reports uncertain cleanup and unique probe account", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{
			setErr: fakeLinuxKeyringErr(secrets.ReasonTimeout, true, "credential store operation timed out after 15s"),
		}
		var out, stderr bytes.Buffer
		rt := &Runtime{Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected failure")
		}
		failure, code := errmap.Details(err)
		if code != output.ExitConfig || failure.Context["cleanup_status"] != secrets.CleanupStatusUncertain || failure.Context["outcome_uncertain"] != true {
			t.Fatalf("unexpected timeout context: code=%d ctx=%+v", code, failure.Context)
		}
		probeAcct, _ := failure.Context["probe_account"].(string)
		if !strings.HasPrefix(probeAcct, secrets.ProbeAccount+"-") || !strings.Contains(failure.Message, probeAcct) {
			t.Fatalf("expected timeout message to identify unique probe account %q: %q", probeAcct, failure.Message)
		}
		var envBuf bytes.Buffer
		if err := output.WriteJSON(&envBuf, output.Failure(rt.current, failure, time.Now())); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.keyring-test.schema.json", envBuf.Bytes())
	})

	t.Run("auth logout delete timeout reports uncertain state and leaves config untouched", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		configPath := filepath.Join(t.TempDir(), "config.toml")
		cfg := &account.Config{
			Schema:         account.ConfigSchema,
			DefaultAccount: "personal",
			Accounts:       map[string]account.Account{"personal": {Email: "user@qq.com"}},
		}
		if err := cfg.Save(configPath); err != nil {
			t.Fatal(err)
		}
		provider := &stubKeyringProvider{
			deleteErr: fakeLinuxKeyringErr(secrets.ReasonTimeout, true, "credential store operation timed out after 15s"),
		}
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "auth", "logout", "--name", "personal"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected logout timeout failure")
		}
		failure, code := errmap.Details(err)
		if code != output.ExitConfig || failure.Context["outcome_uncertain"] != true || failure.Context["local_credential_deleted_state"] != secrets.OutcomeUncertain || failure.Context["config_updated"] != false {
			t.Fatalf("unexpected logout timeout context: code=%d ctx=%+v", code, failure.Context)
		}
		loaded, _, loadErr := account.Load(configPath)
		if loadErr != nil || loaded.DefaultAccount != "personal" {
			t.Fatalf("config should remain intact on uncertain logout delete: err=%v cfg=%+v", loadErr, loaded)
		}
	})

	t.Run("rejects --auth-code-env cleanly without touching keyring", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "0")
		provider := &stubKeyringProvider{}
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "--auth-code-env", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected usage error with --auth-code-env")
		}
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.Usage) || code != output.ExitUsage {
			t.Fatalf("got code=%s exit=%d, want usage/%d", failure.Code, code, output.ExitUsage)
		}
		if provider.setCalls != 0 || provider.getCalls != 0 || provider.deleteCalls != 0 {
			t.Fatalf("provider was touched despite --auth-code-env refusal")
		}
	})

	t.Run("readonly refuses before touching keyring", func(t *testing.T) {
		t.Setenv(policy.ReadonlyEnv, "1")
		provider := &stubKeyringProvider{}
		rt := &Runtime{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "auth", "keyring-test"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected readonly policy error")
		}
		failure, code := errmap.Details(err)
		if failure.Code != string(errmap.PolicyDenied) || code != output.ExitPolicyDenied {
			t.Fatalf("got code=%s exit=%d, want policy_denied/%d", failure.Code, code, output.ExitPolicyDenied)
		}
		if provider.setCalls != 0 || provider.getCalls != 0 || provider.deleteCalls != 0 {
			t.Fatalf("provider was touched under readonly mode")
		}
	})

	t.Run("embedded schema is discoverable via schema command", func(t *testing.T) {
		var out bytes.Buffer
		rt := &Runtime{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader("")}
		root := NewRoot(rt)
		root.SetArgs([]string{"--json", "schema", "auth.keyring-test"})
		if err := root.Execute(); err != nil {
			t.Fatalf("schema auth.keyring-test: %v", err)
		}
		if got := schemaCommandConst(t, out.Bytes()); got != "auth.keyring-test" {
			t.Fatalf("schema command const=%q, want auth.keyring-test", got)
		}
	})
}

func TestAuthStatusSurfacesCredentialStoreErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := &account.Config{
		Schema:         account.ConfigSchema,
		DefaultAccount: "personal",
		Accounts:       map[string]account.Account{"personal": {Email: "user@qq.com"}},
	}
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}

	t.Run("credential missing reports not_found without store error fields", func(t *testing.T) {
		var out bytes.Buffer
		rt := &Runtime{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: &stubKeyringProvider{}}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "auth", "status"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.status.schema.json", out.Bytes())
		var doc struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Data["credential_present"] != false || doc.Data["credential_store_status"] != "not_found" {
			t.Fatalf("unexpected auth status data: %+v", doc.Data)
		}
		if _, exists := doc.Data["credential_store_reason"]; exists {
			t.Fatalf("did not expect credential_store_reason on simple not_found: %+v", doc.Data)
		}
	})

	t.Run("store error surfaces additive reason and suggestion in JSON and text", func(t *testing.T) {
		provider := &stubKeyringProvider{
			getErr: fakeLinuxKeyringErr(secrets.ReasonNoSessionBus, false, "dbus: couldn't determine address of session bus"),
		}
		var out bytes.Buffer
		rt := &Runtime{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: provider}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "auth", "status"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		validateOutput(t, "auth.status.schema.json", out.Bytes())
		var doc struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Data["credential_present"] != false ||
			doc.Data["credential_store_status"] != "error" ||
			doc.Data["credential_store_reason"] != string(secrets.ReasonNoSessionBus) ||
			doc.Data["credential_store_platform"] != "linux" ||
			doc.Data["dbus_session_bus_set"] != false {
			t.Fatalf("unexpected auth status error fields: %+v", doc.Data)
		}
		if s, _ := doc.Data["credential_store_suggestion"].(string); !strings.Contains(s, "gnome-keyring") {
			t.Fatalf("missing actionable suggestion: %+v", doc.Data)
		}

		// Human mode also surfaces the store error and suggestion.
		var textOut bytes.Buffer
		rtText := &Runtime{Out: &textOut, Err: &bytes.Buffer{}, In: strings.NewReader(""), Secrets: provider}
		rootText := NewRoot(rtText)
		rootText.SetArgs([]string{"--config", configPath, "auth", "status"})
		if err := rootText.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(textOut.String(), "credential=false") || !strings.Contains(textOut.String(), "reason=no_session_bus") || !strings.Contains(textOut.String(), "建议：") {
			t.Fatalf("unexpected human auth status output: %q", textOut.String())
		}
	})
}

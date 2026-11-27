package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/secrets"
)

func TestDoctorFailureReportsCompletedCheckNames(t *testing.T) {
	configPath := saveSendConfig(t, nil)
	const fixtureSecret = "synthetic-fixture"
	for _, test := range []struct {
		name, stage, path, code string
		secret                  string
		completed               []string
		wantExit                int
	}{
		{name: "missing config", stage: "config", path: filepath.Join(t.TempDir(), "missing.toml"), code: "config", wantExit: 3},
		{name: "missing credential", stage: "credential", path: configPath, code: "auth_code_missing", completed: []string{"config"}, wantExit: 10},
		{name: "login failure", stage: "imap_login", path: configPath, code: "auth_failed", secret: fixtureSecret, completed: []string{"config", "credential", "dns", "tcp", "tls", "imap_greeting", "imap_capability_before_login"}, wantExit: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			provider := &secrets.Memory{Values: map[string]string{}}
			if test.secret != "" {
				provider.Values["user@qq.com"] = test.secret
			}
			rt := &Runtime{
				Out: &out, Err: io.Discard, Secrets: provider,
				Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
					if test.secret == "" {
						t.Fatal("doctor tried to dial before configuration and credential checks")
					}
					return nil, &errmap.Error{
						Kind: errmap.AuthFailed, Message: "fixture login rejected",
						Context: map[string]any{"stage": "imap_login", "server": "example.invalid:993"},
					}
				},
			}
			root := NewRoot(rt)
			root.SetArgs([]string{"--config", test.path, "--json", "doctor"})
			err := root.Execute()
			if err == nil {
				t.Fatal("doctor succeeded")
			}
			failure, exit := errmap.Details(err)
			if exit != test.wantExit || failure.Code != test.code || failure.Retryable {
				t.Fatalf("incorrect error semantics: exit=%d, error=%+v", exit, failure)
			}
			if err := output.WriteJSON(&out, output.Failure(rt.current, failure, time.Now())); err != nil {
				t.Fatal(err)
			}
			validateOutput(t, "doctor.schema.json", out.Bytes())
			var doc struct {
				OK    bool
				Error struct {
					Context struct {
						Stage     string
						Completed []string `json:"completed_checks"`
						Server    string
					}
				}
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			stages := doc.Error.Context
			if doc.OK || stages.Stage != test.stage || stages.Completed == nil || !slices.Equal(stages.Completed, test.completed) {
				t.Fatalf("incorrect doctor JSON: %s", out.Bytes())
			}
			if test.secret != "" && (stages.Server != "example.invalid:993" || bytes.Contains(out.Bytes(), []byte(fixtureSecret))) {
				t.Fatalf("doctor failed to preserve safe diagnostic data or leaked the fixture credential: %s", out.Bytes())
			}
		})
	}
}

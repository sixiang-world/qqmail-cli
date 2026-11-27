package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/netx"
	"github.com/situker/qqmail-cli/internal/secrets"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

func TestJSONEnvelopeIncludesConnectionMetaAndMatchesSchema(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := &account.Config{
		Schema:         account.ConfigSchema,
		DefaultAccount: "personal",
		Accounts: map[string]account.Account{
			"personal": {
				Email:          "user@qq.com",
				SendAllowlist:  []string{"allowed@example.com"},
				AutoSend:       true,
				DailyAutoLimit: 5,
			},
		},
	}
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	provider := &secrets.Memory{Values: map[string]string{"user@qq.com": "abcdefghijklmnop"}}

	t.Run("IMAP read command records connection telemetry", func(t *testing.T) {
		var out bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: io.Discard, Secrets: provider,
			Dial: func(ctx context.Context, _ account.Named, _ string) (imapx.Reader, error) {
				netx.RecordConnection(ctx, netx.ConnectionInfo{
					Protocol:  "imap",
					Attempts:  2,
					Stage:     "imap_ready",
					Endpoint:  "127.0.0.1:993",
					Server:    "imap.qq.com:993",
					Transport: "implicit_tls",
				})
				return richReader{}, nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "folder", "list"})
		if err := root.Execute(); err != nil {
			t.Fatalf("folder list failed: %v", err)
		}
		validateOutput(t, "folder.list.schema.json", out.Bytes())
		var doc struct {
			Meta struct {
				Connection struct {
					Protocol  string `json:"protocol"`
					Attempts  int    `json:"attempts"`
					Stage     string `json:"stage"`
					Endpoint  string `json:"endpoint"`
					Server    string `json:"server"`
					Transport string `json:"transport"`
				} `json:"connection"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		got := doc.Meta.Connection
		if got.Protocol != "imap" || got.Attempts != 2 || got.Stage != "imap_ready" || got.Endpoint != "127.0.0.1:993" || got.Transport != "implicit_tls" {
			t.Fatalf("unexpected meta.connection: %+v", got)
		}
	})

	t.Run("SMTP send retry preserves single quota reservation and single audit pair", func(t *testing.T) {
		stateFile := filepath.Join(dir, "autosend-state.json")
		oldStatePath := autoSendStatePath
		autoSendStatePath = func() (string, error) { return stateFile, nil }
		t.Cleanup(func() { autoSendStatePath = oldStatePath })

		dbPath := filepath.Join(dir, "audit.db")
		var out bytes.Buffer
		var sendCalls int
		rt := &Runtime{
			Out: &out, Err: io.Discard, Secrets: provider,
			IndexOpen: func(_ string, writable bool) (*index.DB, error) {
				return index.OpenPath(dbPath, writable)
			},
			SendMail: func(ctx context.Context, _ account.Named, _ string, _ sendmail.Draft, _ []byte) error {
				sendCalls++
				netx.RecordConnection(ctx, netx.ConnectionInfo{
					Protocol:     "smtp",
					Attempts:     2,
					Stage:        "smtp_sent",
					Endpoint:     "127.0.0.1:587",
					Server:       "smtp.qq.com:587",
					Transport:    "starttls",
					FallbackUsed: true,
				})
				return nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs([]string{"--config", configPath, "--json", "send", "--to", "allowed@example.com", "--subject", "retry check", "--body", "hello", "--execute"})
		if err := root.Execute(); err != nil {
			t.Fatalf("send failed: %v", err)
		}
		validateOutput(t, "send.schema.json", out.Bytes())
		if sendCalls != 1 {
			t.Fatalf("expected 1 logical send call, got %d", sendCalls)
		}
		rawState, err := os.ReadFile(stateFile)
		if err != nil {
			t.Fatal(err)
		}
		var state autoSendState
		if err := json.Unmarshal(rawState, &state); err != nil {
			t.Fatal(err)
		}
		if state.Count != 1 {
			t.Fatalf("expected autosend quota count 1, got %d", state.Count)
		}
		store, err := index.OpenPath(dbPath, false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		entries, err := store.AuditList(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected exactly 2 audit entries (attempt + result), got %d: %+v", len(entries), entries)
		}
		var doc struct {
			Meta struct {
				Connection struct {
					Protocol     string `json:"protocol"`
					Attempts     int    `json:"attempts"`
					Stage        string `json:"stage"`
					Endpoint     string `json:"endpoint"`
					Transport    string `json:"transport"`
					FallbackUsed bool   `json:"fallback_used"`
				} `json:"connection"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		got := doc.Meta.Connection
		if got.Protocol != "smtp" || got.Attempts != 2 || got.Stage != "smtp_sent" || got.Endpoint != "127.0.0.1:587" || got.Transport != "starttls" || !got.FallbackUsed {
			t.Fatalf("unexpected smtp meta.connection: %+v", got)
		}
	})
}

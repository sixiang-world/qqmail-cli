package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/cleanupplan"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/mimeparse"
	"github.com/situker/qqmail-cli/internal/secrets"
)

// fakeRestoreMutator serves a scripted trash folder: the scan phase reads it
// through the batched Reader surface (Examine/Search/Fetch*), exactly like the
// real client does after the batched-locate rework.
type fakeRestoreMutator struct {
	fakeReader
	uidValidity uint32
	envelopes   []mailmodel.Envelope
	headers     []mailmodel.HeaderFields
	bodies      map[uint32][]byte
}

const scriptedTrashUID = uint32(42)

// newScriptedRestoreMutator publishes raw as one trash message whose size,
// Message-ID, and body fingerprint all derive from the same bytes the backup
// flow saw, so the manifest entry re-identifies it.
func newScriptedRestoreMutator(raw []byte) fakeRestoreMutator {
	parsed := mimeparse.Parse(raw)
	return fakeRestoreMutator{
		uidValidity: 9,
		envelopes:   []mailmodel.Envelope{{UID: scriptedTrashUID, UIDValidity: 9, Size: int64(len(raw))}},
		headers:     []mailmodel.HeaderFields{{UID: scriptedTrashUID, MessageID: parsed.MessageID}},
		bodies:      map[uint32][]byte{scriptedTrashUID: raw},
	}
}

func fixtureMessageRaw() []byte {
	raw, _, err := fakeReader{}.FetchBodyPeek(context.Background(), mailmodel.MsgID{}, 0)
	if err != nil {
		panic(err)
	}
	return raw
}

func (m fakeRestoreMutator) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return []mailmodel.Folder{{Name: "Deleted Messages", Attributes: []string{`\Trash`}}}, nil
}
func (m fakeRestoreMutator) Examine(context.Context, string) (uint32, uint32, error) {
	return m.uidValidity, uint32(len(m.envelopes)), nil
}
func (m fakeRestoreMutator) Search(context.Context, imapx.SearchFilter) ([]uint32, error) {
	ids := make([]uint32, 0, len(m.envelopes))
	for _, envelope := range m.envelopes {
		ids = append(ids, envelope.UID)
	}
	return ids, nil
}
func (m fakeRestoreMutator) FetchEnvelopes(_ context.Context, _ string, _ uint32, ids []uint32) ([]mailmodel.Envelope, error) {
	want := map[uint32]bool{}
	for _, id := range ids {
		want[id] = true
	}
	result := []mailmodel.Envelope{}
	for _, envelope := range m.envelopes {
		if want[envelope.UID] {
			result = append(result, envelope)
		}
	}
	return result, nil
}
func (m fakeRestoreMutator) FetchHeaderFields(_ context.Context, ids []uint32) ([]mailmodel.HeaderFields, error) {
	want := map[uint32]bool{}
	for _, id := range ids {
		want[id] = true
	}
	result := []mailmodel.HeaderFields{}
	for _, header := range m.headers {
		if want[header.UID] {
			result = append(result, header)
		}
	}
	return result, nil
}
func (m fakeRestoreMutator) FetchBodyPeek(_ context.Context, id mailmodel.MsgID, maxBytes int64) ([]byte, bool, error) {
	if raw, ok := m.bodies[id.UID]; ok {
		return raw, false, nil
	}
	return m.fakeReader.FetchBodyPeek(context.Background(), id, maxBytes)
}
func (m fakeRestoreMutator) SetSeen(context.Context, mailmodel.MsgID) error { return nil }
func (m fakeRestoreMutator) SetFlags(context.Context, mailmodel.MsgID, []string, []string) error {
	return nil
}
func (m fakeRestoreMutator) MoveUID(context.Context, mailmodel.MsgID, string) (imapx.MutationResult, error) {
	return imapx.MutationResult{Method: "uid_move", DestinationVerified: true}, nil
}
func (m fakeRestoreMutator) CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, imapx.MessageIdentity) (imapx.MutationResult, error) {
	return imapx.MutationResult{Method: "copy_store_deleted", SourceRetained: true}, nil
}
func (m fakeRestoreMutator) LocateByIdentity(_ context.Context, folder string, _ imapx.MessageIdentity) ([]mailmodel.MsgID, error) {
	return []mailmodel.MsgID{{Folder: folder, UIDValidity: m.uidValidity, UID: scriptedTrashUID}}, nil
}

// restoreFixtureWithRaw runs the real backup flow against a scripted source
// message so plan.json gains a backup_root with a verified HMAC manifest —
// restore refuses anything less. The same raw backs the trash fixture.
func restoreFixtureWithRaw(t *testing.T, dialRaw []byte) (string, string, *secrets.Memory) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := &account.Config{Schema: account.ConfigSchema, DefaultAccount: "personal", Accounts: map[string]account.Account{"personal": {Email: "user@qq.com"}}}
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	provider := &secrets.Memory{Values: map[string]string{"user@qq.com": "abcdefghijklmnop"}}
	planPath := filepath.Join(dir, "plan.json")
	id := mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 1}.String()
	if err := cleanupplan.Save(planPath, cleanupplan.Plan{
		Schema: 1, CreatedAt: time.Now().UTC(),
		Items:      []cleanupplan.Item{{ID: id, Category: "marketing", Reason: "fixture", Evidence: []string{"fixture"}, From: []mailmodel.Address{}, Subject: "fixture", Date: time.Now().UTC(), SizeBytes: 1}},
		Statistics: cleanupplan.Statistics{TotalCount: 1, TotalSizeBytes: 1, ByCategory: map[string]int{"marketing": 1}, ByFromDomain: map[string]int{"(unknown)": 1}},
	}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider,
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			return constBodyReader{raw: dialRaw}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "backup", "--plan", planPath, "--output", filepath.Join(dir, "backup")})
	if err := root.Execute(); err != nil {
		t.Fatalf("backup fixture failed: %v (stderr=%s)", err, stderr.String())
	}
	return configPath, planPath, provider
}

func restoreFixture(t *testing.T) (string, string, *secrets.Memory) {
	return restoreFixtureWithRaw(t, fixtureMessageRaw())
}

// constBodyReader serves one fixed raw message for every fetch, standing in
// for a mailbox that contains exactly one source message.
type constBodyReader struct {
	fakeReader
	raw []byte
}

func (r constBodyReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return r.raw, false, nil
}

func TestRestoreDryRunMatchesSchemaAndLocates(t *testing.T) {
	configPath, planPath, provider := restoreFixture(t)
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider,
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return newScriptedRestoreMutator(fixtureMessageRaw()), nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "restore", "--plan", planPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("restore dry-run failed: %v (stderr=%s)", err, stderr.String())
	}
	validateOutput(t, "restore.schema.json", out.Bytes())
	var envelope struct {
		Data struct {
			DryRun  bool             `json:"dry_run"`
			Located []map[string]any `json:"located"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Data.DryRun || len(envelope.Data.Located) != 1 {
		t.Fatalf("unexpected restore dry-run: %s", out.String())
	}
	if envelope.Data.Located[0]["match"] != "message_id" {
		t.Fatalf("expected message_id match, got %v", envelope.Data.Located[0]["match"])
	}
}

func TestRestoreExecuteMovesBackAndMatchesSchema(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	configPath, planPath, provider := restoreFixture(t)
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader("1\n"), Secrets: provider,
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return newScriptedRestoreMutator(fixtureMessageRaw()), nil
		},
		IndexOpen:  func(string, bool) (*index.DB, error) { return index.OpenPath(filepath.Join(t.TempDir(), "cache.db"), true) },
		IsTerminal: func(io.Reader) bool { return true },
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "restore", "--plan", planPath, "--execute"})
	if err := root.Execute(); err != nil {
		t.Fatalf("restore execute failed: %v (stderr=%s)", err, stderr.String())
	}
	validateOutput(t, "restore.schema.json", out.Bytes())
	var envelope struct {
		Data struct {
			Completed int `json:"completed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Completed != 1 {
		t.Fatalf("unexpected restore execute output: %s", out.String())
	}
}

// The clean gate deliberately admits Message-ID-less wild mail (gated on
// UIDVALIDITY+UID+RFC822.SIZE). Restore must still find those messages in the
// trash: the scan falls back to exact-size candidates confirmed by full-body
// SHA-256 against the verified backup manifest.
func TestRestoreLocatesMessageWithoutMessageIDBySHA256(t *testing.T) {
	raw := []byte("From: sender@example.com\r\nSubject: no message id\r\n\r\nwild body without a Message-ID header\r\n")
	configPath, planPath, provider := restoreFixtureWithRaw(t, raw)
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider,
		DialMutator: func(context.Context, account.Named, string) (imapx.Mutator, error) {
			return newScriptedRestoreMutator(raw), nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "--json", "restore", "--plan", planPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("restore dry-run failed: %v (stderr=%s)", err, stderr.String())
	}
	validateOutput(t, "restore.schema.json", out.Bytes())
	var envelope struct {
		Data struct {
			Located  []map[string]any `json:"located"`
			NotFound []map[string]any `json:"not_found"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Located) != 1 || len(envelope.Data.NotFound) != 0 {
		t.Fatalf("expected the Message-ID-less message to be located by fingerprint: %s", out.String())
	}
	if envelope.Data.Located[0]["match"] != "sha256" {
		t.Fatalf("expected sha256 match, got %v", envelope.Data.Located[0]["match"])
	}
}

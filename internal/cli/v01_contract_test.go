package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/secrets"
	projectschemas "github.com/situker/qqmail-cli/schemas"
)

// richReader adds realistic list/search results on top of fakeReader so the
// v0.1 read commands produce populated payloads for schema validation.
type richReader struct{ fakeReader }

func (richReader) ListFolders(context.Context) ([]mailmodel.Folder, error) {
	return []mailmodel.Folder{{Name: "INBOX", Delimiter: "/", Attributes: []string{}}}, nil
}
func (richReader) Search(context.Context, imapx.SearchFilter) ([]uint32, error) {
	return []uint32{1}, nil
}
func (richReader) FetchEnvelopes(_ context.Context, folder string, validity uint32, ids []uint32) ([]mailmodel.Envelope, error) {
	result := []mailmodel.Envelope{}
	for _, uid := range ids {
		result = append(result, mailmodel.Envelope{
			ID: mailmodel.MsgID{Folder: folder, UIDValidity: validity, UID: uid}.String(), UID: uid, UIDValidity: validity,
			Folder: folder, Subject: "fixture", From: []mailmodel.Address{{Email: "sender@example.com"}}, To: []mailmodel.Address{}, Flags: []string{}, Size: 90,
		})
	}
	return result, nil
}

// Every v0.1 read command's real --json output must pass its schema — these
// are the highest-traffic agent surfaces.
func TestV01ReadCommandsMatchSchemas(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := &account.Config{Schema: account.ConfigSchema, DefaultAccount: "personal", Accounts: map[string]account.Account{"personal": {Email: "user@qq.com"}}}
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	provider := &secrets.Memory{Values: map[string]string{"user@qq.com": "abcdefghijklmnop"}}
	id := mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 1}.String()
	downloadDir := filepath.Join(dir, "downloads")
	exportDir := filepath.Join(dir, "export")
	cases := []struct {
		args   []string
		schema string
	}{
		{[]string{"--config", configPath, "--json", "auth", "status"}, "auth.status.schema.json"},
		{[]string{"--config", configPath, "--json", "account", "list"}, "account.list.schema.json"},
		{[]string{"--config", configPath, "--json", "account", "use", "personal"}, "account.use.schema.json"},
		{[]string{"--config", configPath, "--json", "doctor"}, "doctor.schema.json"},
		{[]string{"--config", configPath, "--json", "folder", "list"}, "folder.list.schema.json"},
		{[]string{"--config", configPath, "--json", "envelope", "list"}, "envelope.list.schema.json"},
		{[]string{"--config", configPath, "--json", "message", "show", id}, "message.show.schema.json"},
		{[]string{"--config", configPath, "--json", "attachment", "list", id}, "attachment.list.schema.json"},
		{[]string{"--config", configPath, "--json", "attachment", "download", id, "all", "--output", downloadDir}, "attachment.download.schema.json"},
		{[]string{"--config", configPath, "--json", "export", "--ids", id, "--output", exportDir}, "export.schema.json"},
		{[]string{"--config", configPath, "--json", "auth", "logout", "--name", "personal"}, "auth.logout.schema.json"},
	}
	for _, tc := range cases {
		var out, stderr bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: &stderr, In: strings.NewReader(""), Secrets: provider,
			Dial: func(context.Context, account.Named, string) (imapx.Reader, error) { return richReader{}, nil },
		}
		root := NewRoot(rt)
		root.SetArgs(tc.args)
		if err := root.Execute(); err != nil {
			t.Fatalf("args %v failed: %v (stderr=%s)", tc.args, err, stderr.String())
		}
		validateOutput(t, tc.schema, out.Bytes())
	}
	// The manifest export just wrote must itself pass the manifest schema.
	manifestRaw, err := os.ReadFile(filepath.Join(exportDir, "personal", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	validateDocument(t, "manifest.schema.json", manifestRaw)
}

// validateDocument checks a standalone JSON document (not an envelope) against
// an embedded schema file.
func validateDocument(t *testing.T, schemaName string, raw []byte) {
	t.Helper()
	document, err := projectschemas.Get(schemaName)
	if err != nil {
		t.Fatal(err)
	}
	var schemaValue, value any
	if err := json.Unmarshal(document, &schemaValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	resource := "https://github.com/situker/qqmail-cli/schemas/" + schemaName
	if err := compiler.AddResource(resource, schemaValue); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(value); err != nil {
		t.Fatalf("%s: %v", schemaName, err)
	}
}

// Task 2/10/11 annotated email-derived fields (content_id, body_preview,
// html_source_excerpt) in the shared schemas. The untrusted_paths cross-check
// only fires when an annotation exists, so silently deleting one would fail
// nowhere else — these assertions pin the annotations themselves.
func TestEmailDerivedSchemaAnnotationsStayRegistered(t *testing.T) {
	cases := []struct {
		schema   string
		property string
	}{
		{"attachment.list.schema.json", "content_id"},
		{"message.show.schema.json", "content_id"},
		{"send.schema.json", "content_id"},
		{"send.schema.json", "body_preview"},
		{"send.schema.json", "html_source_excerpt"},
	}
	for _, tc := range cases {
		raw, err := projectschemas.Get(tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		annotated := map[string]bool{}
		collectUntrustedProperties(document, "", annotated)
		if !annotated[tc.property] {
			t.Errorf("%s lost its UNTRUSTED annotation on %q", tc.schema, tc.property)
		}
	}
	// reply/forward share send's result shape by reference; a duplicated copy
	// would let the three shapes drift apart.
	for _, name := range []string{"reply.schema.json", "forward.schema.json"} {
		raw, err := projectschemas.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"send.schema.json#/$defs/composeResult"`) {
			t.Errorf("%s no longer $refs send.schema.json#/$defs/composeResult", name)
		}
	}
}

// The 0.4 compose additions must stay schema-visible end to end: the
// --body-format html / --attach-inline dry-run summary carries the derived
// preview fields and the generated content_id, while --save-draft routes its
// result through the shared mutation shape — all validate against the same
// send.schema.json $defs that reply and forward reference.
func TestV04ComposeOutputMatchesSharedSchemas(t *testing.T) {
	png := filepath.Join(t.TempDir(), "logo.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCLIReader(t, fakeReader{}, "send", "--to", "reader@example.com", "--subject", "s",
		"--body-format", "html", "--body", `<p>hi</p><img src="cid:logo.png@qqmail-cli.local">`, "--attach-inline", png, "--json")
	validateOutput(t, "send.schema.json", []byte(out))
	mustContain(t, out, `"body_preview"`, `"html_source_excerpt"`, `"content_id"`)

	stub := &saveDraftMutatorStub{}
	out = runCLIReader(t, stub, "send", "--to", "reader@example.com", "--subject", "s", "--body", "b", "--save-draft", "--json")
	validateOutput(t, "send.schema.json", []byte(out))
	mustContain(t, out, `"action":"save_draft"`, `"destination":"Drafts"`)

	out = runCLIReader(t, stub, "reply", inboxIDString, "--body", "thanks", "--save-draft", "--json")
	validateOutput(t, "reply.schema.json", []byte(out))
	mustContain(t, out, `"action":"save_draft"`)
}

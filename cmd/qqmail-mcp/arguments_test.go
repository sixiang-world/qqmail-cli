package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/cli"
	"github.com/spf13/cobra"
)

// Exercise the real CLI command tree and flag definitions without contacting
// a mailbox: replace only the leaf action after constructing the CLI.
func TestSearchArgumentsPreserveLiteralQuery(t *testing.T) {
	profiles := []struct {
		name, mode string
		folder     string
		limit      int
		before     uint32
		generation uint32
	}{
		{name: "default", limit: 50},
		{name: "server-page", mode: "server", folder: "已发送邮件/Sent Messages", limit: 7, before: 99, generation: 777},
		{name: "default-mode-folder", folder: "--help", limit: 50},
	}
	for _, profile := range profiles {
		for _, query := range []string{"-invoice", "--help", "--", "--server=false", "发票 with spaces"} {
			t.Run(profile.name+"/"+query, func(t *testing.T) {
				input := map[string]any{"query": query}
				if profile.mode != "" {
					input["mode"], input["limit"] = profile.mode, profile.limit
				}
				if profile.folder != "" {
					input["folder"] = profile.folder
				}
				if profile.before != 0 {
					input["before_uid"], input["expected_uidvalidity"] = profile.before, profile.generation
				}
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				args, err := toolArguments("qqmail_search", raw)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				rt := &cli.Runtime{Out: &out, Err: &out}
				root := cli.NewRoot(rt)
				leaf, _, err := root.Find([]string{"search"})
				if err != nil {
					t.Fatal(err)
				}
				called := false
				leaf.RunE = func(cmd *cobra.Command, positional []string) error {
					called = true
					if !reflect.DeepEqual(positional, []string{query}) {
						t.Errorf("query changed: %q", positional)
					}
					server, _ := cmd.Flags().GetBool("server")
					local, _ := cmd.Flags().GetBool("local")
					limit, _ := cmd.Flags().GetInt("limit")
					before, _ := cmd.Flags().GetUint32("before-uid")
					generation, _ := cmd.Flags().GetUint32("expected-uidvalidity")
					if !server || local || limit != profile.limit || before != profile.before || generation != profile.generation {
						t.Errorf("flags changed: server=%v local=%v limit=%d before=%d generation=%d", server, local, limit, before, generation)
					}
					return nil
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if !called || !rt.JSON || rt.Timeout != 45*time.Second {
					t.Fatalf("search action not reached with required global flags: called=%v json=%v timeout=%v output=%s", called, rt.JSON, rt.Timeout, out.String())
				}
				wantFolder := profile.folder
				if wantFolder == "" {
					wantFolder = "INBOX"
				}
				if rt.Folder != wantFolder {
					t.Errorf("folder=%q, want %q", rt.Folder, wantFolder)
				}
			})
		}
	}
}

func TestSearchRejectsInvalidFolder(t *testing.T) {
	for _, raw := range []string{
		`{"query":"invoice","folder":null}`,
		`{"query":"invoice","folder":42}`,
		`{"query":"invoice","folder":""}`,
		`{"query":"invoice","folder":"  "}`,
		`{"query":"invoice","folder":"invalid\u0000name"}`,
		`{"query":"invoice","folder":"invalid\r\nname"}`,
	} {
		if _, err := toolArguments("qqmail_search", json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid search folder in %s", raw)
		}
	}
}

func TestMessageArgumentsPreserveLiteralID(t *testing.T) {
	for _, tool := range []struct{ name, command, subcommand string }{
		{"qqmail_show", "message", "show"},
		{"qqmail_attachments", "attachment", "list"},
	} {
		for _, id := range []string{"--help", "--", "m1_opaque"} {
			t.Run(tool.name+"/"+id, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]string{"id": id})
				args, err := toolArguments(tool.name, raw)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				root := cli.NewRoot(&cli.Runtime{Out: &out, Err: &out})
				leaf, _, err := root.Find([]string{tool.command, tool.subcommand})
				if err != nil {
					t.Fatal(err)
				}
				called := false
				leaf.RunE = func(_ *cobra.Command, positional []string) error {
					called = true
					if !reflect.DeepEqual(positional, []string{id}) {
						t.Errorf("message ID changed: %q", positional)
					}
					return nil
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatalf("message action not reached: %s", out.String())
				}
			})
		}
	}
}

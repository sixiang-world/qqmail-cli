package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func params(s string) json.RawMessage { return json.RawMessage(s) }

func TestToolArgumentsReadOnly(t *testing.T) {
	cases := []struct {
		Name string
		Args string
		Want string
	}{
		{"qqmail_folders", "{}", "folder list"},
		{"qqmail_list", `{"unread":true,"limit":5,"before_uid":100}`, "--before-uid 100"},
		{"qqmail_show", `{"id":"m1_opaque"}`, "message show -- m1_opaque"},
		{"qqmail_search", `{"query":"发票","before_uid":77}`, "--before-uid 77"},
		{"qqmail_attachments", `{"id":"m1_opaque"}`, "attachment list -- m1_opaque"},
	}
	for _, test := range cases {
		args, err := toolArguments(test.Name, params(test.Args))
		if err != nil {
			t.Fatalf("%s: %v", test.Name, err)
		}
		all := strings.Join(args, " ")
		if !strings.Contains(all, test.Want) {
			t.Fatalf("%s: %s lacks %s", test.Name, all, test.Want)
		}
		for _, a := range args {
			if a == "--execute" || a == "send" || a == "delete" {
				t.Fatalf("unsafe args: %q", args)
			}
		}
	}
}

func TestRejectUnknownOrUnsafeArgs(t *testing.T) {
	cases := []struct{ name, arguments string }{
		{"qqmail_folders", `{"command":"send"}`},
		{"qqmail_show", `{"id":"x","execute":true}`},
		{"qqmail_search", `{"query":"x","mode":"local"}`},
		{"qqmail_search", `{"query":"x","limit":101}`},
		{"qqmail_list", `{"limit":-1}`},
		{"qqmail_list", `{"limit":0}`},
		{"qqmail_list", `{"expected_uidvalidity":0}`},
		{"qqmail_search", `{"query":"a","mode":null}`},
		{"qqmail_search", `{"query":"x","limit":0}`},
		{"qqmail_search", `{"query":"x","limit":1.5}`},
		{"qqmail_show", `{"id":null}`},
		{"qqmail_list", `{"unread":"true"}`},
		{"unknown", "{}"},
	}
	for _, test := range cases {
		if _, err := toolArguments(test.name, params(test.arguments)); err == nil {
			t.Fatalf("accepted %s %s", test.name, test.arguments)
		}
	}
}

func TestCallToolFakeCLIEnvelope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture; Windows coverage via cross-platform executable fixture pending")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-cli")
	script := `#!/bin/sh
case "$QQMAIL_CLI_READONLY:$*" in
  "1:--json --timeout 45s folder list") ;;
  *) exit 99 ;;
esac
case "$TEST_CASE" in
 partial) echo '{"ok":true,"data":{"messages":[{"body":"kept"}]},"warnings":[{"code":"parse_error","message":"partial"}]}'; exit 70 ;;
 failure) echo '{"ok":false,"data":null,"error":{"code":"delivery_uncertain","retryable":false,"message":"do not retry"},"warnings":[]}'; exit 20 ;;
 broken) echo '{"ok":true'; exit 70 ;;
 empty) exit 1 ;;
 *) echo '{"ok":true,"data":{"folders":[]},"warnings":[]}' ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QQMAIL_CLI_BIN", path)
	for _, tc := range []struct {
		label   string
		failure bool
		needle  string
	}{
		{"partial", false, "kept"},
		{"failure", true, "delivery_uncertain"},
		{"broken", true, "invalid JSON"},
		{"empty", true, "no JSON"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Setenv("TEST_CASE", tc.label)
			v := callTool(context.Background(), "qqmail_folders", params("{}"))
			raw, _ := json.Marshal(v)
			if !bytes.Contains(raw, []byte(tc.needle)) {
				t.Fatalf("lost envelope: %s", raw)
			}
			if got := v.(map[string]any)["isError"]; got != tc.failure {
				t.Fatalf("isError=%v: %s", got, raw)
			}
		})
	}
}

func TestMCPStdioInitialization(t *testing.T) {
	for _, offered := range []string{"2025-06-18", "2025-11-25", "2025-03-26"} {
		t.Run(offered, func(t *testing.T) {
			input := strings.Join([]string{
				`{"jsonrpc":"2.0","id":"invalid","method":"initialize","params":{}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":"early-call","method":"tools/call","params":{"name":"qqmail_folders"}}`,
				`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"` + offered + `","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
				`{"jsonrpc":"2.0","id":"early-list","method":"tools/list"}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":"ready","method":"tools/list"}`,
				`{"jsonrpc":"2.0","id":"local","method":"tools/call","params":{"name":"qqmail_search","arguments":{"query":"fixture","mode":"local"}}}`,
				`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
			}, "\n")
			var out bytes.Buffer
			if err := serve(strings.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			want := [][]string{
				{`"id":"invalid"`, `"code":-32602`},
				{`"id":"early-call"`, `"code":-32002`},
				{`"id":"init"`, `"protocolVersion":"2025-06-18"`},
				{`"id":"early-list"`, `"code":-32002`},
				{`"id":"ready"`, `"enum":["server"]`, `"name":"qqmail_search"`},
				{`"id":"local"`, `"isError":true`, "only server search is supported"},
				{`"id":null`, `"code":-32600`},
			}
			if len(lines) != len(want) {
				t.Fatalf("expected %d responses, got %d: %s", len(want), len(lines), out.String())
			}
			for i, line := range lines {
				if !json.Valid([]byte(line)) || !strings.Contains(line, `"jsonrpc":"2.0"`) {
					t.Fatalf("invalid MCP response: %s", line)
				}
				for _, part := range want[i] {
					if !strings.Contains(line, part) {
						t.Errorf("response %d missing %q: %s", i, part, line)
					}
				}
			}
		})
	}
}

func TestInvalidRequestID(t *testing.T) {
	var out bytes.Buffer
	if err := serve(strings.NewReader(`{"jsonrpc":"2.0","id":{},"method":"ping"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":-32600`) {
		t.Fatal(out.String())
	}
}

func TestMCPGenerationPreconditionArguments(t *testing.T) {
	for _, name := range []string{"qqmail_list", "qqmail_search"} {
		raw := `{"expected_uidvalidity":777}`
		if name == "qqmail_search" {
			raw = `{"query":"invoice","expected_uidvalidity":777}`
		}
		args, err := toolArguments(name, params(raw))
		if err != nil || !strings.Contains(strings.Join(args, " "), "--expected-uidvalidity 777") {
			t.Fatalf("%s missing generation guard: %v %v", name, args, err)
		}
	}
}

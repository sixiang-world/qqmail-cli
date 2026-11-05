package main

import (
    "encoding/json"
    "bytes"
    "context"
    "os"
    "path/filepath"
    "runtime"
    "time"
    "io"
    "strings"
    "testing"
)

func params(s string) json.RawMessage { return json.RawMessage(s) }

func TestToolArgumentsReadOnly(t *testing.T) {
    cases:=[]struct{Name string; Args string; Want string}{
        {"qqmail_folders","{}","folder list"},
        {"qqmail_list",`{"unread":true,"limit":5,"before_uid":100}`,"--before-uid 100"},
        {"qqmail_show",`{"id":"m1_opaque"}`,"message show m1_opaque"},
        {"qqmail_search",`{"query":"发票","before_uid":77}`,"--before-uid 77"},
        {"qqmail_attachments",`{"id":"m1_opaque"}`,"attachment list m1_opaque"},
    }
    for _,test:=range cases {
        args,err:=toolArguments(test.Name,params(test.Args))
        if err!=nil {t.Fatalf("%s: %v",test.Name,err)}
        all:=strings.Join(args," ")
        if !strings.Contains(all,test.Want) {t.Fatalf("%s: %s lacks %s",test.Name,all,test.Want)}
        for _,a:=range args {if a=="--execute"||a=="send"||a=="delete"{t.Fatalf("unsafe args: %q",args)}}
    }
}

func TestRejectUnknownOrUnsafeArgs(t *testing.T) {
    cases:=[]struct{name,arguments string}{
        {"qqmail_folders",`{"command":"send"}`},
        {"qqmail_show",`{"id":"x","execute":true}`},
        {"qqmail_search",`{"query":"x","mode":"local","before_uid":4}`},
        {"qqmail_search",`{"query":"x","limit":101}`},
        {"qqmail_list",`{"limit":-1}`},
        {"qqmail_list",`{"limit":0}`},
        {"qqmail_search",`{"query":"x","limit":0}`},
        {"qqmail_search",`{"query":"x","limit":1.5}`},
        {"qqmail_show",`{"id":null}`},
        {"qqmail_list",`{"unread":"true"}`},
        {"unknown","{}"},
    }
    for _,test:=range cases {
        if _,err:=toolArguments(test.name,params(test.arguments));err==nil {t.Fatalf("accepted %s %s",test.name,test.arguments)}
    }
}

func TestCallToolFakeCLIEnvelope(t *testing.T) {
	if runtime.GOOS=="windows"{t.Skip("POSIX shell fixture; Windows coverage via cross-platform executable fixture pending")}
	dir:=t.TempDir()
	path:=filepath.Join(dir,"fake-cli")
	script:=`#!/bin/sh
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
	if err:=os.WriteFile(path,[]byte(script),0o700);err!=nil{t.Fatal(err)}
	t.Setenv("QQMAIL_CLI_BIN",path)
	for _,tc:=range []struct{label string;failure bool;needle string}{
		{"partial",false,"kept"},
		{"failure",true,"delivery_uncertain"},
		{"broken",true,"invalid JSON"},
		{"empty",true,"no JSON"},
	} {
		t.Run(tc.label,func(t *testing.T){
			t.Setenv("TEST_CASE",tc.label)
			v:=callTool(context.Background(),"qqmail_folders",params("{}"))
			raw,_:=json.Marshal(v)
			if !bytes.Contains(raw,[]byte(tc.needle)){t.Fatalf("lost envelope: %s",raw)}
			if got:=v.(map[string]any)["isError"];got!=tc.failure{t.Fatalf("isError=%v: %s",got,raw)}
		})
	}
}

func TestMCPStdioRequestAndNotifications(t *testing.T) {
	in:=strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":"two","method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"qqmail_list","arguments":{"limit":0}}}
{"jsonrpc":"2.0","id":4,"method":"unknown"}`)
	var out bytes.Buffer
	if err:=serve(in,&out);err!=nil{t.Fatal(err)}
	lines:=strings.Split(strings.TrimSpace(out.String()),"\n")
	if len(lines)!=4{t.Fatalf("expected four responses, got %d: %s",len(lines),out.String())}
	if strings.Contains(out.String(),"notifications/initialized"){t.Fatalf("notification generated a response: %s",out.String())}
	for _,line:=range lines{
		var m map[string]any
		if err:=json.Unmarshal([]byte(line),&m);err!=nil{t.Fatal(err)}
		if m["jsonrpc"]!="2.0"{t.Fatalf("missing JSONRPC: %s",line)}
	}
}

func TestMCPDisconnectCancelsHangingCLI(t *testing.T) {
	if runtime.GOOS=="windows"{t.Skip("POSIX script fixture")}
	dir:=t.TempDir()
	path:=filepath.Join(dir,"hang-cli")
	if err:=os.WriteFile(path,[]byte("#!/bin/sh\nexec sleep 30\n"),0o700);err!=nil{t.Fatal(err)}
	t.Setenv("QQMAIL_CLI_BIN",path)
	r,w:=io.Pipe()
	var out bytes.Buffer
	done:=make(chan error,1)
	go func(){done<-serve(r,&out)}()
	_,_=io.WriteString(w,`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qqmail_folders","arguments":{}}}`+"\n")
	// Keep the command in flight while EOF is delivered.
	time.Sleep(50*time.Millisecond)
	_=w.Close()
	select {
	case err:=<-done:if err!=nil{t.Fatal(err)}
	case <-time.After(3*time.Second):t.Fatal("stdio EOF left a subprocess running")
	}
}

func TestInvalidRequestID(t *testing.T) {
	var out bytes.Buffer
	if err:=serve(strings.NewReader(`{"jsonrpc":"2.0","id":{},"method":"ping"}`+"\n"),&out);err!=nil{t.Fatal(err)}
	if !strings.Contains(out.String(),`"code":-32600`){t.Fatal(out.String())}
}

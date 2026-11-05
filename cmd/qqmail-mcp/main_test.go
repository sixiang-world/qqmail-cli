package main

import (
    "encoding/json"
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
        {"qqmail_list",`{"unread":"true"}`},
        {"unknown","{}"},
    }
    for _,test:=range cases {
        if _,err:=toolArguments(test.name,params(test.arguments));err==nil {t.Fatalf("accepted %s %s",test.name,test.arguments)}
    }
}

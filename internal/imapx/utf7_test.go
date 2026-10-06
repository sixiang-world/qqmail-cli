package imapx

import "testing"

// Task 8's wire capture (mutate_folders_test.go) recorded a real go-imap
// serialization of "2026-账单" as 2026-&jSZTVQ-; the known-vector tests pin
// EncodeMailbox/DecodeMailbox to exactly that form so hand-built commands and
// display paths stay in agreement with go-imap's serializer.
func TestEncodeMailboxKnownVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"INBOX", "INBOX"},
		{"2026-账单", "2026-&jSZTVQ-"},
		{"账单", "&jSZTVQ-"},
		{"a&b", "a&-b"},
		{"&", "&-"},
		{"chart.png", "chart.png"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := EncodeMailbox(tc.in); got != tc.want {
			t.Errorf("EncodeMailbox(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeMailboxKnownVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"INBOX", "INBOX"},
		{"2026-&jSZTVQ-", "2026-账单"},
		{"a&-b", "a&b"},
		{"chart.png", "chart.png"},
		{"", ""},
	}
	for _, tc := range cases {
		got, err := DecodeMailbox(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("DecodeMailbox(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

func TestDecodeMailboxRejectsMalformed(t *testing.T) {
	for _, in := range []string{"&jSZTVQ", "&,,,,-"} {
		if _, err := DecodeMailbox(in); err == nil {
			t.Errorf("DecodeMailbox(%q) = nil error, want failure", in)
		}
	}
}

func TestModifiedUTF7RoundTrip(t *testing.T) {
	for _, value := range []string{"INBOX", "Sent Messages", "客户邮件", "研发&测试", "已删除邮件", "草稿箱", "2026-账单/子文件夹", "邮件 & 备份", "图表.png", " ~~ Odd ~~ "} {
		encoded := EncodeMailbox(value)
		got, err := DecodeMailbox(encoded)
		if err != nil || got != value {
			t.Fatalf("%q -> %q -> %q (%v)", value, encoded, got, err)
		}
	}
}

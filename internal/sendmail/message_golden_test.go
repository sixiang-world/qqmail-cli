package sendmail

import (
	"encoding/base64"
	"net/mail"
	"regexp"
	"testing"
	"time"
)

// The base64 blobs below were captured from Build() before the --body-format
// change landed (see the generation note in message_test.go). They lock the
// pre-existing plain-text wire format byte for byte: BodyFormat "" and "text"
// must produce exactly the old output, including line endings, header order,
// and quoted-printable encoding. The only fields that are random by design —
// the Message-ID and multipart boundaries — are masked on both sides before
// comparison.
const (
	goldenPlainNoAttachment = "RGF0ZTogRnJpLCAwMiBKYW4gMjAyNiAwMzowNDowNSArMDAwMA0KTWVzc2FnZS1JRDogPDcwZjVlNzMzMzY0MTkzODI0NmRmNjExOGRhYjI0ZmQ1QHFxLmNvbT4NCkZyb206ID0/dXRmLTg/cT89RTU9OEY9OTE9RTQ9QkI9QjY9RTQ9QkE9QkE/PSA8c2VuZGVyQHFxLmNvbT4NClRvOiA9P3V0Zi04P3E/PUU2PTk0PUI2PUU0PUJCPUI2PUU0PUJBPUJBPz0gPHJlYWRlckBleGFtcGxlLmNvbT4NClN1YmplY3Q6ID0/VVRGLTg/cT89RTQ9Qjg9QUQ9RTY9OTY9ODc9RTQ9Qjg9QkI9RTk9QTI9OTg/PQ0KTUlNRS1WZXJzaW9uOiAxLjANCkNjOiA8Y2NAZXhhbXBsZS5jb20+DQpJbi1SZXBseS1UbzogPGZpeHR1cmVAZXhhbXBsZS5jb20+DQpSZWZlcmVuY2VzOiA8b2xkZXJAZXhhbXBsZS5jb20+DQpDb250ZW50LVR5cGU6IHRleHQvcGxhaW47IGNoYXJzZXQ9IlVURi04Ig0KQ29udGVudC1UcmFuc2Zlci1FbmNvZGluZzogcXVvdGVkLXByaW50YWJsZQ0KDQpIZWxsbyA9RTQ9QkQ9QTA9RTU9QTU9QkQNClNlY29uZCBsaW5lDQoNCj1FNz1BQz1BQz1FNT05Qj05Qj1FOD1BMT04QyBib2R5Lg0K"
	goldenPlainMixed        = "RGF0ZTogRnJpLCAwMiBKYW4gMjAyNiAwMzowNDowNSArMDAwMA0KTWVzc2FnZS1JRDogPGE4MzZlOTcwMGVkYzdmNzUyZjRmMWYxYjU5YTI2MGRmQHFxLmNvbT4NCkZyb206ID0/dXRmLTg/cT89RTU9OEY9OTE9RTQ9QkI9QjY9RTQ9QkE9QkE/PSA8c2VuZGVyQHFxLmNvbT4NClRvOiA9P3V0Zi04P3E/PUU2PTk0PUI2PUU0PUJCPUI2PUU0PUJBPUJBPz0gPHJlYWRlckBleGFtcGxlLmNvbT4NClN1YmplY3Q6ID0/VVRGLTg/cT89RTQ9Qjg9QUQ9RTY9OTY9ODc9RTQ9Qjg9QkI9RTk9QTI9OTg/PQ0KTUlNRS1WZXJzaW9uOiAxLjANCkNjOiA8Y2NAZXhhbXBsZS5jb20+DQpJbi1SZXBseS1UbzogPGZpeHR1cmVAZXhhbXBsZS5jb20+DQpSZWZlcmVuY2VzOiA8b2xkZXJAZXhhbXBsZS5jb20+DQpDb250ZW50LVR5cGU6IG11bHRpcGFydC9taXhlZDsgYm91bmRhcnk9ImRmMGM0ZGIzOGM1MmE5YmI5MGMwYjM1OTcyY2IwNzQ3MzNkZDMyZjIzNzk3OThjMmRhYjY1MDU0NmNkNSINCg0KLS1kZjBjNGRiMzhjNTJhOWJiOTBjMGIzNTk3MmNiMDc0NzMzZGQzMmYyMzc5Nzk4YzJkYWI2NTA1NDZjZDUNCkNvbnRlbnQtVHJhbnNmZXItRW5jb2Rpbmc6IHF1b3RlZC1wcmludGFibGUNCkNvbnRlbnQtVHlwZTogdGV4dC9wbGFpbjsgY2hhcnNldD0iVVRGLTgiDQoNCkhlbGxvID1FND1CRD1BMD1FNT1BNT1CRA0KU2Vjb25kIGxpbmUNCg0KPUU3PUFDPUFDPUU1PTlCPTlCPUU4PUExPThDIGJvZHkuDQoNCi0tZGYwYzRkYjM4YzUyYTliYjkwYzBiMzU5NzJjYjA3NDczM2RkMzJmMjM3OTc5OGMyZGFiNjUwNTQ2Y2Q1DQpDb250ZW50LURpc3Bvc2l0aW9uOiBhdHRhY2htZW50OyBmaWxlbmFtZSo9dXRmLTgnJyVFNiVCNSU4QiVFOCVBRiU5NSVFNiU4QSVBNSVFNSU5MSU4QS5wZGYNCkNvbnRlbnQtVHJhbnNmZXItRW5jb2Rpbmc6IGJhc2U2NA0KQ29udGVudC1UeXBlOiBhcHBsaWNhdGlvbi9wZGY7IG5hbWUqPXV0Zi04JyclRTYlQjUlOEIlRTglQUYlOTUlRTYlOEElQTUlRTUlOTElOEEucGRmDQoNClptbDRkSFZ5WlE9PQ0KDQotLWRmMGM0ZGIzOGM1MmE5YmI5MGMwYjM1OTcyY2IwNzQ3MzNkZDMyZjIzNzk3OThjMmRhYjY1MDU0NmNkNS0tDQo="
)

var (
	goldenMessageIDRE = regexp.MustCompile(`Message-ID: <[0-9a-f]+@`)
	goldenBoundaryRE  = regexp.MustCompile(`boundary="[0-9a-f]{20,}"`)
	goldenBoundaryLn  = regexp.MustCompile(`(?m)^--[0-9a-f]{20,}(--)?\r?$`)
)

func normalizeNondeterministic(raw []byte) string {
	value := goldenMessageIDRE.ReplaceAllString(string(raw), "Message-ID: <golden-id@")
	value = goldenBoundaryRE.ReplaceAllString(value, `boundary="golden-boundary"`)
	return goldenBoundaryLn.ReplaceAllString(value, "--golden-boundary${1}")
}

func TestBuildPlainTextPathIsByteIdentical(t *testing.T) {
	base := Draft{
		From:       mail.Address{Name: "发件人", Address: "sender@qq.com"},
		To:         []mail.Address{{Name: "收件人", Address: "reader@example.com"}},
		Cc:         []mail.Address{{Address: "cc@example.com"}},
		Subject:    "中文主题",
		Body:       "Hello 你好\nSecond line\r\n\n第四行 body.\n",
		InReplyTo:  "fixture@example.com",
		References: []string{"older@example.com"},
		Date:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	for _, tc := range []struct {
		name     string
		format   string
		attach   bool
		golden64 string
	}{
		{"empty format, body only", "", false, goldenPlainNoAttachment},
		{"explicit text, body only", "text", false, goldenPlainNoAttachment},
		{"empty format, with attachment", "", true, goldenPlainMixed},
		{"explicit text, with attachment", "text", true, goldenPlainMixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := base
			draft.BodyFormat = tc.format
			if tc.attach {
				draft.Attachments = []Attachment{{Filename: "测试报告.pdf", ContentType: "application/pdf", Data: []byte("fixture")}}
			}
			raw, err := Build(draft)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			golden, err := base64.StdEncoding.DecodeString(tc.golden64)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := normalizeNondeterministic(raw), normalizeNondeterministic(golden); got != want {
				t.Fatalf("plain-text Build output drifted from the pre-change golden\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}

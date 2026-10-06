package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/secrets"
)

// headersOnlyReader serves a message that carries no text part at all (only a
// binary attachment), so the text mode of `message show` must render a
// placeholder instead of the literal "<nil>" that fmt.Sprint produces for a
// nil body.
type headersOnlyReader struct{ fakeReader }

func (headersOnlyReader) FetchBodyPeek(context.Context, mailmodel.MsgID, int64) ([]byte, bool, error) {
	return []byte("From: sender@example.com\r\n" +
		"Subject: no text part\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"data.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC\r\n" +
		"--b1--\r\n"), false, nil
}

func TestMessageShowTextModeRendersMissingBodyPlaceholder(t *testing.T) {
	configPath := saveSendConfig(t, nil)
	var out, stderr bytes.Buffer
	rt := &Runtime{
		Out: &out, Err: &stderr, In: strings.NewReader(""),
		Secrets: &secrets.Memory{Values: map[string]string{"user@qq.com": testAuthCode}},
		Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
			return headersOnlyReader{}, nil
		},
	}
	root := NewRoot(rt)
	root.SetArgs([]string{"--config", configPath, "message", "show", "m1_eyJmIjoiSU5CT1giLCJ2IjoxLCJ1IjoxfQ"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<nil>") {
		t.Fatalf("nil body rendered as <nil>: %s", out.String())
	}
	if !strings.Contains(out.String(), "(no text body)") {
		t.Fatalf("missing-body placeholder absent: %s", out.String())
	}
}

package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/errmap"
)

// The count/token confirmations are the last human gate in front of every
// destructive action; all three states of each must hold.
func TestConfirmExactCountThreeStates(t *testing.T) {
	gate := func(input string, terminal bool) error {
		rt := &Runtime{Err: &bytes.Buffer{}, In: strings.NewReader(input), IsTerminal: func(io.Reader) bool { return terminal }}
		return confirmExactCount(rt, 3)
	}
	if err := gate("3\n", true); err != nil {
		t.Fatalf("correct count rejected: %v", err)
	}
	if err := gate("2\n", true); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("wrong count accepted: %v", err)
	}
	if err := gate("", true); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("EOF/empty input accepted: %v", err)
	}
	if err := gate("3\n", false); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("non-TTY accepted: %v", err)
	}
}

func TestConfirmTokenThreeStates(t *testing.T) {
	gate := func(input string, terminal bool) error {
		rt := &Runtime{Err: &bytes.Buffer{}, In: strings.NewReader(input), IsTerminal: func(io.Reader) bool { return terminal }}
		return confirmToken(rt, "CLEAR")
	}
	if err := gate("CLEAR\n", true); err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
	// The token match is case-insensitive (the send family accepts "send"),
	// so the same word in another case passes; only a different word fails.
	if err := gate("clear\n", true); err != nil {
		t.Fatalf("same word in another case rejected: %v", err)
	}
	if err := gate("no\n", true); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("different word accepted: %v", err)
	}
	if err := gate("", true); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("EOF/empty input accepted: %v", err)
	}
	if err := gate("CLEAR\n", false); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("non-TTY accepted: %v", err)
	}
}

// The send confirmation token semantics are "type send (any case)": the same
// word passes in any letter casing, while y/ok/empty/other words and non-TTY
// stdin are still refused — the token is not a reflex key.
func TestConfirmTokenCaseInsensitive(t *testing.T) {
	gate := func(input, token string, terminal bool) error {
		rt := &Runtime{Err: &bytes.Buffer{}, In: strings.NewReader(input), IsTerminal: func(io.Reader) bool { return terminal }}
		return confirmToken(rt, token)
	}
	for _, input := range []string{"SEND\n", "send\n", "Send\n", "  send  \n"} {
		if err := gate(input, "SEND", true); err != nil {
			t.Fatalf("input %q rejected: %v", input, err)
		}
	}
	for _, input := range []string{"y\n", "ok\n", "yes\n", "\n", "   \n", ""} {
		if err := gate(input, "SEND", true); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
			t.Fatalf("input %q accepted: %v", input, err)
		}
	}
	if err := gate("send\n", "SEND", false); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("non-TTY accepted: %v", err)
	}
}

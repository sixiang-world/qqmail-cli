package imapx

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"

	"github.com/situker/qqmail-cli/internal/errmap"
)

// A refused SELECT is usually a wrong folder name, but QQ's rate limiting
// presents as the same failure (docs/compat/qq-20260902.md §4a). The kind must
// separate permanent not-found from transient conditions so agents can trust
// the exit code.
func TestClassifySelectError(t *testing.T) {
	for _, tc := range []struct {
		name string
		cause error
		want errmap.Kind
	}{
		{"rate_limited", errors.New("EXAMINE failed: too many connections, try later"), errmap.RateLimited},
		{"rate_limited_text", errors.New("EXAMINE failed: temporarily blocked due to frequency"), errmap.RateLimited},
		{"dropped_connection", io.ErrUnexpectedEOF, errmap.Network},
		{"timeout", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, errmap.Network},
		{"unknown_folder", errors.New("EXAMINE failed: Mailbox does not exist"), errmap.NotFound},
		{"generic_refusal", errors.New("EXAMINE failed: unspecified NO"), errmap.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifySelectError(tc.cause, "INBOX")
			got := errmap.Classify(err).Kind
			if got != tc.want {
				t.Fatalf("classifySelectError(%v) = %s, want %s", tc.cause, got, tc.want)
			}
		})
	}
}

package imapx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/situker/qqmail-cli/internal/errmap"
)

// Pinned classification semantics (docs/compat/qq-20261006.md, pending the
// Task 0 probe archive): only a
// completed server rejection — a tagged NO/BAD status response — maps to
// policy_denied. Network-class errors pass through unchanged so the existing
// retryable classification survives untouched.
func TestWrapSearchRejectMapsOnlyCompletedServerRejections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  errmap.Kind
	}{
		{"tagged_no", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "unsupported criterion"}, errmap.PolicyDenied},
		{"tagged_bad", &imap.Error{Type: imap.StatusResponseTypeBad, Text: "invalid command"}, errmap.PolicyDenied},
		{"wrapped_no", fmt.Errorf("UID SEARCH failed: %w", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "no"}), errmap.PolicyDenied},
		{"ok_is_not_a_rejection", &imap.Error{Type: imap.StatusResponseTypeOK, Text: "search done"}, errmap.Internal},
		{"network_passthrough", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, errmap.Network},
		{"plain_error_passthrough", io.ErrUnexpectedEOF, errmap.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WrapSearchReject(tc.cause)
			if kind := errmap.Classify(got).Kind; kind != tc.want {
				t.Fatalf("WrapSearchReject(%v) classified as %s, want %s", tc.cause, kind, tc.want)
			}
			if tc.want != errmap.PolicyDenied {
				return
			}
			// The mapped rejection must keep pointing at the dialect record and
			// the local-search fallback: wording drift here strands the user
			// without the documented degradation path.
			var mapped *errmap.Error
			if !errors.As(got, &mapped) {
				t.Fatalf("WrapSearchReject(%v) = %T, want *errmap.Error", tc.cause, got)
			}
			if !strings.Contains(mapped.Message, "docs/compat/") {
				t.Fatalf("rejection message lost the docs/compat/ reference: %q", mapped.Message)
			}
			if !strings.Contains(mapped.Suggestion, "search --local") {
				t.Fatalf("rejection suggestion lost the search --local fallback: %q", mapped.Suggestion)
			}
		})
	}
}

func TestWrapSearchRejectReturnsNetworkErrorsUnchanged(t *testing.T) {
	networkErr := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	if WrapSearchReject(networkErr) != error(networkErr) {
		t.Fatal("network error was rewrapped instead of returned unchanged")
	}
	if WrapSearchReject(nil) != nil {
		t.Fatal("nil input must stay nil")
	}
}

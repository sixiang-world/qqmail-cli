package imapx

import (
	"errors"

	imap "github.com/emersion/go-imap/v2"
	"github.com/situker/qqmail-cli/internal/errmap"
)

// WrapSearchReject converts a server NO/BAD rejection of a SEARCH command into
// the PolicyDenied application error. Some servers (QQ included, see
// docs/compat/qq-20261006.md) refuse search criteria they do not support; the
// tagged status text survives as the cause so --verbose keeps the server's
// own words. Network-class errors pass through unchanged, leaving the existing
// retryable classification untouched.
func WrapSearchReject(err error) error {
	var imapErr *imap.Error
	if errors.As(err, &imapErr) && (imapErr.Type == imap.StatusResponseTypeNo || imapErr.Type == imap.StatusResponseTypeBad) {
		return &errmap.Error{Kind: errmap.PolicyDenied,
			Message:    "服务器拒绝了这个搜索条件（见 docs/compat/qq-20261006.md）",
			Suggestion: "改用 --from/--subject 过滤，或 qqmail-cli sync 后 search --local", Cause: err}
	}
	return err
}

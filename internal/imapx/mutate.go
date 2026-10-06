package imapx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"

	imap "github.com/emersion/go-imap/v2"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/mailmodel"
)

// Mutator extends the read surface with the only server mutations allowed by
// qqmail-cli. Command code must reach these methods through internal/policy.
// LocateByIdentity is itself read-only (EXAMINE + UID SEARCH + PEEK fetches);
// it lives here because its only consumers are mutation flows (restore, and
// the conservative copy confirmation).
type Mutator interface {
	Reader
	SetSeen(context.Context, mailmodel.MsgID) error
	SetFlags(context.Context, mailmodel.MsgID, []string, []string) error
	MoveUID(context.Context, mailmodel.MsgID, string) (MutationResult, error)
	CopyMarkDeletedUID(context.Context, mailmodel.MsgID, string, MessageIdentity) (MutationResult, error)
	LocateByIdentity(context.Context, string, MessageIdentity) ([]mailmodel.MsgID, error)
}

type MessageIdentity struct {
	MessageID string
	SizeBytes int64
	// SHA256 is the fingerprint of the exact message bytes from the verified
	// backup. It is the fallback identity for wild messages that legitimately
	// lack a Message-ID header — the clean gate admits those on
	// UIDVALIDITY+UID+RFC822.SIZE, so restore and the conservative-copy
	// confirmation need a way to re-identify them without the header.
	SHA256 string
}

type MutationResult struct {
	Method              string `json:"method"`
	Destination         string `json:"destination,omitempty"`
	DestinationVerified bool   `json:"destination_verified,omitempty"`
	SourceMarkedDeleted bool   `json:"source_marked_deleted,omitempty"`
	SourceRetained      bool   `json:"source_retained,omitempty"`
}

func DialMutatorWithVersion(ctx context.Context, cfg account.Named, authCode, version string) (Mutator, error) {
	return DialWithVersion(ctx, cfg, authCode, version)
}

func (c *Client) SetSeen(ctx context.Context, id mailmodel.MsgID) error {
	if err := c.selectWritable(ctx, id); err != nil {
		return err
	}
	if err := c.setDeadline(ctx); err != nil {
		return err
	}
	stop := c.watchdog(ctx)
	defer stop()
	command := c.raw.Store(imap.UIDSetNum(imap.UID(id.UID)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil)
	return command.Close()
}

// SetFlags adds or removes flags on one message. Exactly one of add/remove
// must be non-empty; policy layer owns the \Flagged/\Seen vocabulary.
func (c *Client) SetFlags(ctx context.Context, id mailmodel.MsgID, add, remove []string) error {
	if (len(add) == 0) == (len(remove) == 0) {
		return &errmap.Error{Kind: errmap.Usage, Message: "SetFlags 要求 add/remove 恰一非空"}
	}
	if err := c.selectWritable(ctx, id); err != nil {
		return err
	}
	if err := c.setDeadline(ctx); err != nil {
		return err
	}
	stop := c.watchdog(ctx)
	defer stop()
	op, flags := imap.StoreFlagsAdd, toFlagList(add)
	if len(remove) > 0 {
		op, flags = imap.StoreFlagsDel, toFlagList(remove) // beta.8 常量名是 StoreFlagsDel
	}
	command := c.raw.Store(imap.UIDSetNum(imap.UID(id.UID)), &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil)
	return command.Close()
}

func toFlagList(names []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(names))
	for _, n := range names {
		out = append(out, imap.Flag(n))
	}
	return out
}

func (c *Client) MoveUID(ctx context.Context, id mailmodel.MsgID, destination string) (MutationResult, error) {
	if !hasCapability(c.capAfter, "MOVE") {
		return MutationResult{}, &errmap.Error{Kind: errmap.PolicyDenied, Message: "服务器未声明 MOVE；拒绝调用可能降级为不安全流程的库方法"}
	}
	if err := c.selectWritable(ctx, id); err != nil {
		return MutationResult{}, err
	}
	if err := c.setDeadline(ctx); err != nil {
		return MutationResult{}, err
	}
	stop := c.watchdog(ctx)
	defer stop()
	if _, err := c.raw.Move(imap.UIDSetNum(imap.UID(id.UID)), destination).Wait(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Method: "uid_move", Destination: destination, DestinationVerified: true}, nil
}

func (c *Client) CopyMarkDeletedUID(ctx context.Context, id mailmodel.MsgID, destination string, identity MessageIdentity) (MutationResult, error) {
	if err := c.selectWritable(ctx, id); err != nil {
		return MutationResult{}, err
	}
	if err := c.setDeadline(ctx); err != nil {
		return MutationResult{}, err
	}
	stopCopy := c.watchdog(ctx)
	copyData, err := c.raw.Copy(imap.UIDSetNum(imap.UID(id.UID)), destination).Wait()
	stopCopy()
	if err != nil {
		return MutationResult{}, err
	}
	confirmed := false
	if copyData != nil && copyData.UIDValidity > 0 {
		sourceUIDs, sourceOK := copyData.SourceUIDs.Nums()
		if destinationUIDs, ok := copyData.DestUIDs.Nums(); sourceOK && ok && len(sourceUIDs) == 1 && sourceUIDs[0] == imap.UID(id.UID) && len(destinationUIDs) == 1 {
			confirmed = true
		}
	}
	if !confirmed {
		confirmed, err = c.confirmDestination(ctx, destination, identity)
		if err != nil {
			return MutationResult{}, err
		}
	}
	if !confirmed {
		return MutationResult{}, &errmap.Error{Kind: errmap.PolicyDenied, Message: "无法按 Message-ID 与 RFC822.SIZE 确认目标夹副本；源邮件保持不变"}
	}
	if err := c.selectWritable(ctx, id); err != nil {
		return MutationResult{}, err
	}
	if err := c.setDeadline(ctx); err != nil {
		return MutationResult{}, err
	}
	stopStore := c.watchdog(ctx)
	defer stopStore()
	command := c.raw.Store(imap.UIDSetNum(imap.UID(id.UID)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil)
	if err := command.Close(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Method: "copy_store_deleted", Destination: destination, DestinationVerified: true, SourceMarkedDeleted: true, SourceRetained: true}, nil
}

func (c *Client) selectWritable(ctx context.Context, id mailmodel.MsgID) error {
	if err := c.setDeadline(ctx); err != nil {
		return err
	}
	stop := c.watchdog(ctx)
	defer stop()
	selected, err := c.raw.Select(id.Folder, &imap.SelectOptions{ReadOnly: false}).Wait()
	if err != nil {
		return err
	}
	if selected.UIDValidity != id.UIDValidity {
		return &errmap.Error{Kind: errmap.StaleID, Message: "邮件 id 已失效：文件夹 UIDVALIDITY 已变化", Context: map[string]any{"folder": id.Folder}}
	}
	return nil
}

func (c *Client) confirmDestination(ctx context.Context, destination string, identity MessageIdentity) (bool, error) {
	matches, err := c.LocateByIdentity(ctx, destination, identity)
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

// LocateByIdentity finds messages in a folder. It is read-only and powers both
// the conservative-copy confirmation and the restore command's trash lookup.
// With a Message-ID it matches on Message-ID plus RFC822.SIZE; without one it
// falls back to exact RFC822.SIZE candidates confirmed by full-body SHA-256 —
// wild email legitimately lacks the Message-ID header, and those messages must
// still be re-identifiable against the verified backup fingerprint.
func (c *Client) LocateByIdentity(ctx context.Context, folder string, identity MessageIdentity) ([]mailmodel.MsgID, error) {
	if identity.SizeBytes < 0 {
		return []mailmodel.MsgID{}, nil
	}
	uidValidity, _, err := c.Examine(ctx, folder)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(identity.MessageID) != "" {
		criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: identity.MessageID}}}
		data, err := c.uidSearch(ctx, criteria)
		if err != nil {
			return nil, err
		}
		all := data.AllUIDs()
		if len(all) == 0 {
			return []mailmodel.MsgID{}, nil
		}
		ids := make([]uint32, len(all))
		for i, uid := range all {
			ids[i] = uint32(uid)
		}
		envelopes, err := c.FetchEnvelopes(ctx, folder, uidValidity, ids)
		if err != nil {
			return nil, err
		}
		headers, err := c.FetchHeaderFields(ctx, ids)
		if err != nil {
			return nil, err
		}
		headerByUID := map[uint32]string{}
		for _, header := range headers {
			headerByUID[header.UID] = normalizeMessageID(header.MessageID)
		}
		matches := []mailmodel.MsgID{}
		for _, envelope := range envelopes {
			if envelope.Size == identity.SizeBytes && headerByUID[envelope.UID] == normalizeMessageID(identity.MessageID) {
				matches = append(matches, mailmodel.MsgID{Folder: folder, UIDValidity: uidValidity, UID: envelope.UID})
			}
		}
		return matches, nil
	}
	if identity.SHA256 != "" {
		return c.locateByFingerprint(ctx, folder, uidValidity, identity)
	}
	return []mailmodel.MsgID{}, nil
}

// fingerprintCandidateCap bounds the full-body refetches one fingerprint lookup
// may issue: exact-size collisions in a folder are rare, and an unbounded scan
// would trip QQ's connection-rate limits (docs/compat/qq-20260902.md §4a).
// A capped miss reports "not found", which is the safe direction for both
// callers — the source message stays untouched, and restore keeps the local
// backup as the remaining copy.
const fingerprintCandidateCap = 16

func (c *Client) locateByFingerprint(ctx context.Context, folder string, uidValidity uint32, identity MessageIdentity) ([]mailmodel.MsgID, error) {
	data, err := c.uidSearch(ctx, &imap.SearchCriteria{})
	if err != nil {
		return nil, err
	}
	all := data.AllUIDs()
	if len(all) == 0 {
		return []mailmodel.MsgID{}, nil
	}
	ids := make([]uint32, len(all))
	for i, uid := range all {
		ids[i] = uint32(uid)
	}
	envelopes, err := c.FetchEnvelopes(ctx, folder, uidValidity, ids)
	if err != nil {
		return nil, err
	}
	matches := []mailmodel.MsgID{}
	fetched := 0
	for _, envelope := range envelopes {
		if envelope.Size != identity.SizeBytes {
			continue
		}
		if fetched >= fingerprintCandidateCap {
			break
		}
		fetched++
		raw, truncated, err := c.FetchBodyPeek(ctx, mailmodel.MsgID{Folder: folder, UIDValidity: uidValidity, UID: envelope.UID}, MaxMessageBytes)
		if err != nil || truncated {
			continue
		}
		if strings.EqualFold(SHA256Hex(raw), identity.SHA256) {
			matches = append(matches, mailmodel.MsgID{Folder: folder, UIDValidity: uidValidity, UID: envelope.UID})
		}
	}
	return matches, nil
}

// uidSearch runs one UID SEARCH under the standard deadline/watchdog pair.
func (c *Client) uidSearch(ctx context.Context, criteria *imap.SearchCriteria) (*imap.SearchData, error) {
	if err := c.setDeadline(ctx); err != nil {
		return nil, err
	}
	stop := c.watchdog(ctx)
	defer stop()
	return c.raw.UIDSearch(criteria, nil).Wait()
}

// SHA256Hex is the canonical fingerprint form used by backup manifests and
// identity matching.
func SHA256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func classifySelectError(cause error, folder string) error {
	contextData := map[string]any{"folder": folder}
	var networkError net.Error
	if errors.As(cause, &networkError) || errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
		return &errmap.Error{Kind: errmap.Network, Message: "无法以只读方式打开文件夹", Suggestion: "若刚刚频繁登录，请等待 10-15 分钟后重试", Context: contextData, Cause: cause}
	}
	lower := strings.ToLower(cause.Error())
	if strings.Contains(lower, "too many") || strings.Contains(lower, "rate") || strings.Contains(lower, "frequency") || strings.Contains(lower, "temporarily blocked") {
		return &errmap.Error{Kind: errmap.RateLimited, Message: "QQ 邮箱暂时拒绝了打开文件夹的请求", Suggestion: "停止重试并等待 10-15 分钟", Context: contextData, Cause: cause}
	}
	return &errmap.Error{Kind: errmap.NotFound, Message: "无法以只读方式打开文件夹", Context: contextData, Cause: cause}
}

func hasCapability(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func normalizeMessageID(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "<>"))
}

var _ Mutator = (*Client)(nil)

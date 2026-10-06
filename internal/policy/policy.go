package policy

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/index"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/sendmail"
)

const ReadonlyEnv = "QQMAIL_CLI_READONLY"

type Service struct {
	writer imapx.Mutator
	audit  *index.DB
}

type MailTransport func(context.Context, account.Named, string, sendmail.Draft, []byte) error

type MailService struct {
	sender MailTransport
	audit  *index.DB
}

func NewMailService(sender MailTransport, audit *index.DB) *MailService {
	return &MailService{sender: sender, audit: audit}
}

func (s *MailService) Send(ctx context.Context, named account.Named, authCode string, draft sendmail.Draft, raw []byte, command string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	if s.sender == nil || s.audit == nil {
		return fmt.Errorf("mail transport and audit store are required")
	}
	if _, err := s.audit.RecordAudit(ctx, index.AuditEntry{Command: command, Action: "send_attempt", Result: "attempt"}); err != nil {
		return err
	}
	err := s.sender(ctx, named, authCode, draft, raw)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if _, auditErr := s.audit.RecordAudit(ctx, index.AuditEntry{Command: command, Action: "send", Result: result}); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

func New(writer imapx.Mutator, audit *index.DB) *Service {
	return &Service{writer: writer, audit: audit}
}

// Readonly fails closed: any value that is not an explicit falsy token counts
// as readonly. A user who set QQMAIL_CLI_READONLY=enabled (or misspelled a
// truthy value) clearly wanted protection — silently disabling it would be the
// dangerous direction.
func Readonly() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(ReadonlyEnv)))
	switch value {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func RequireMutationAllowed() error {
	if Readonly() {
		return &errmap.Error{Kind: errmap.PolicyDenied, Message: "QQMAIL_CLI_READONLY 已启用，拒绝所有写操作", Suggestion: "仅在人工在场并确认风险后，于该次命令环境中关闭只读开关"}
	}
	return nil
}

func (s *Service) MarkRead(ctx context.Context, id mailmodel.MsgID, command, planRef string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	if err := s.record(ctx, command, "mark_read_attempt", id.String(), "attempt", planRef); err != nil {
		return err
	}
	err := s.writer.SetSeen(ctx, id)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, "mark_read", id.String(), result, planRef); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

func (s *Service) MarkUnread(ctx context.Context, id mailmodel.MsgID, command, planRef string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	if err := s.record(ctx, command, "mark_unread_attempt", id.String(), "attempt", planRef); err != nil {
		return err
	}
	err := s.writer.SetFlags(ctx, id, nil, []string{"\\Seen"})
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, "mark_unread", id.String(), result, planRef); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

// FlagMessage adds or removes \Flagged (QQ 星标). Starred mail is absolutely
// excluded from clean plans, so removal lowers protection and the CLI says so.
func (s *Service) FlagMessage(ctx context.Context, id mailmodel.MsgID, add bool, command, planRef string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	action := "flag_add"
	if !add {
		action = "flag_remove"
	}
	if err := s.record(ctx, command, action+"_attempt", id.String(), "attempt", planRef); err != nil {
		return err
	}
	var err error
	if add {
		err = s.writer.SetFlags(ctx, id, []string{"\\Flagged"}, nil)
	} else {
		err = s.writer.SetFlags(ctx, id, nil, []string{"\\Flagged"})
	}
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, action, id.String(), result, planRef); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

func (s *Service) Move(ctx context.Context, id mailmodel.MsgID, destination string, identity imapx.MessageIdentity, command, planRef string) (imapx.MutationResult, error) {
	if err := RequireMutationAllowed(); err != nil {
		return imapx.MutationResult{}, err
	}
	if err := s.record(ctx, command, "move_attempt", id.String(), "attempt", planRef); err != nil {
		return imapx.MutationResult{}, err
	}
	_, after := s.writer.Capabilities()
	var result imapx.MutationResult
	var err error
	if capability(after, "MOVE") {
		result, err = s.writer.MoveUID(ctx, id, destination)
	} else {
		result, err = s.writer.CopyMarkDeletedUID(ctx, id, destination, identity)
	}
	status := "ok"
	if err != nil {
		status = "failed"
	}
	if auditErr := s.record(ctx, command, "move", id.String(), status+":"+result.Method, planRef); auditErr != nil && err == nil {
		return result, auditErr
	}
	return result, err
}

// CreateFolder and RenameFolder are the folder-structure write verbs. They do
// not depend on any message selection state; the INBOX guard lives in the
// command layer (RFC 3501 gives RENAME INBOX the special semantics of moving
// every message into the new folder). The audit record's id field carries the
// folder name, since folders have no message id.
func (s *Service) CreateFolder(ctx context.Context, name, command string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	err := s.writer.CreateFolder(ctx, name)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, "folder_create", name, result, ""); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

func (s *Service) RenameFolder(ctx context.Context, oldName, newName, command string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	err := s.writer.RenameFolder(ctx, oldName, newName)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, "folder_rename", oldName+"->"+newName, result, ""); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

// SaveDraft APPENDs a fully built message to the server drafts folder under
// the \Draft flag. It is a mutation, not a send: no send allowlist applies,
// but the readonly gate and the attempt/ok audit pair do. The audit record's
// id field stays empty — a draft has no message id until the server assigns
// one, and the content-relevance principle is unaffected.
func (s *Service) SaveDraft(ctx context.Context, folder string, raw []byte, command, planRef string) error {
	if err := RequireMutationAllowed(); err != nil {
		return err
	}
	if err := s.record(ctx, command, "save_draft_attempt", "", "attempt", planRef); err != nil {
		return err
	}
	err := s.writer.AppendDraft(ctx, folder, raw)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if auditErr := s.record(ctx, command, "save_draft", "", result, planRef); auditErr != nil && err == nil {
		return auditErr
	}
	return err
}

func (s *Service) record(ctx context.Context, command, action, id, result, planRef string) error {
	if s.audit == nil {
		return fmt.Errorf("policy audit store is required")
	}
	_, err := s.audit.RecordAudit(ctx, index.AuditEntry{Command: command, Action: action, MsgID: id, Result: result, PlanRef: planRef})
	return err
}

func capability(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

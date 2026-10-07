package cli

import (
	"fmt"
	"net/mail"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/sendmail"
	"github.com/spf13/cobra"
)

// draftFile mirrors one TOML letter for --draft-file: the file carries the
// whole compose input so the invocation needs no other compose flag. Paths
// (body_file/attach/attach_inline) resolve from the current directory, same
// as --body-file/--attach.
type draftFile struct {
	To           []string `toml:"to"`
	Cc           []string `toml:"cc"`
	Bcc          []string `toml:"bcc"`
	Subject      string   `toml:"subject"`
	Format       string   `toml:"format"`
	Body         string   `toml:"body"`
	BodyFile     string   `toml:"body_file"`
	Attach       []string `toml:"attach"`
	AttachInline []string `toml:"attach_inline"`
}

// composeFlagNames lists the compose flags --draft-file is mutually exclusive
// with. There is no merge/override semantics: the file carries the whole
// letter, so any compose flag on the command line is a usage error. The
// action flags --execute and --save-draft stay combinable.
var composeFlagNames = []string{"to", "cc", "bcc", "subject", "body", "body-file", "attach", "attach-inline", "body-format"}

// rejectComposeFlagConflicts refuses --draft-file combined with any compose
// flag. It runs before the draft file is loaded, so the conflict is reported
// even when the path does not exist.
func rejectComposeFlagConflicts(cmd *cobra.Command) error {
	for _, name := range composeFlagNames {
		if cmd.Flags().Changed(name) {
			return &errmap.Error{Kind: errmap.Usage, Message: fmt.Sprintf("--draft-file 与 --%s 不能同时使用", name), Suggestion: "草稿文件承载整封信的全部内容（to/cc/bcc/subject/format/body/body_file/attach/attach_inline），不做合并或覆盖；与 --draft-file 同用的只有 --execute 与 --save-draft"}
		}
	}
	return nil
}

// rejectDraftFileForThreadedCommand refuses --draft-file on reply/forward:
// their recipients and thread headers come from the original mail, and a
// draft file carries its own recipients — merging the two is exactly the
// undefined semantics the mutual-exclusion rule exists to prevent (the same
// reason reply rejects --to).
func rejectDraftFileForThreadedCommand(command string) error {
	return &errmap.Error{Kind: errmap.Usage, Message: fmt.Sprintf("%s 不支持 --draft-file：收件人与线程头来自原邮件，无法与草稿文件合并", command), Suggestion: "独立撰写的新信件请用 send --draft-file；回复/转发正文请用 --body/--body-file 提供"}
}

// loadDraftFile reads and validates one TOML draft file. Validation mirrors
// the flag path: to and subject are required, format is whitelisted by
// validateBodyFormat, and body/body_file are mutually exclusive (composeBody
// enforces the same rule plus the 1 MiB cap when the body is assembled).
func loadDraftFile(path string) (draftFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return draftFile{}, &errmap.Error{Kind: errmap.Usage, Message: fmt.Sprintf("无法读取草稿文件 %s", path), Suggestion: "路径按当前工作目录解析；请确认文件存在", Cause: err}
	}
	var df draftFile
	if err := toml.Unmarshal(raw, &df); err != nil {
		return draftFile{}, &errmap.Error{Kind: errmap.Usage, Message: fmt.Sprintf("草稿文件 %s 不是有效的 TOML", path), Cause: err}
	}
	if len(df.To) == 0 {
		return draftFile{}, &errmap.Error{Kind: errmap.Usage, Message: "草稿文件缺少收件人（to = [\"地址\"]）"}
	}
	if strings.TrimSpace(df.Subject) == "" {
		return draftFile{}, &errmap.Error{Kind: errmap.Usage, Message: "草稿文件缺少主题（subject）"}
	}
	if err := validateBodyFormat(df.Format); err != nil {
		return draftFile{}, err
	}
	if df.Body != "" && df.BodyFile != "" {
		return draftFile{}, &errmap.Error{Kind: errmap.Usage, Message: "草稿文件的 body 与 body_file 只能二选一"}
	}
	return df, nil
}

// draftFromDraftFile assembles the same sendmail.Draft the flag path builds:
// identical loaders (parseAddresses/composeBody/loadAttachments/
// loadInlineAttachments), identical limits and identical errors, so every
// downstream gate in runDraft sees an indistinguishable draft.
func draftFromDraftFile(named account.Named, df draftFile) (sendmail.Draft, error) {
	to, err := parseAddresses(df.To)
	if err != nil {
		return sendmail.Draft{}, err
	}
	cc, err := parseAddresses(df.Cc)
	if err != nil {
		return sendmail.Draft{}, err
	}
	bcc, err := parseAddresses(df.Bcc)
	if err != nil {
		return sendmail.Draft{}, err
	}
	body, err := composeBody(df.Body, df.BodyFile)
	if err != nil {
		return sendmail.Draft{}, err
	}
	domain, err := senderDomain(named.Email)
	if err != nil {
		return sendmail.Draft{}, err
	}
	attachments, err := loadAttachments(df.Attach)
	if err != nil {
		return sendmail.Draft{}, err
	}
	inlines, err := loadInlineAttachments(df.AttachInline, df.Format, domain, attachmentBytes(attachments))
	if err != nil {
		return sendmail.Draft{}, err
	}
	return sendmail.Draft{From: mail.Address{Address: named.Email}, To: to, Cc: cc, Bcc: bcc, Subject: df.Subject, Body: body, BodyFormat: df.Format, Attachments: attachments, Inlines: inlines}, nil
}

package sendmail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/situker/qqmail-cli/internal/output"
	"github.com/situker/qqmail-cli/internal/safeio"
)

const (
	MaxTotalAttachmentBytes int64 = 20 << 20
	MaxRecipientsPerMessage       = 10
	MaxBodyBytes                  = 1 << 20

	// maxHTMLSourceExcerptBytes caps the escaped HTML source excerpt carried
	// in the dry-run summary so the preview stays bounded for any body size.
	maxHTMLSourceExcerptBytes = 2048
)

type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	// ContentID is the bare Content-ID (no angle brackets) for inline parts;
	// Build wraps it in <...> when writing the Content-Id header.
	ContentID string `json:"content_id,omitempty"`
	Data      []byte `json:"-"`
}

type Draft struct {
	From        mail.Address
	To          []mail.Address
	Cc          []mail.Address
	Bcc         []mail.Address
	Subject     string
	Body        string
	BodyFormat  string
	Attachments []Attachment
	// Inlines are the CID-referenced parts (inline images) of an HTML body;
	// they require BodyFormat "html" and become a multipart/related wrapper.
	Inlines    []Attachment
	InReplyTo  string
	References []string
	Date       time.Time
}

type AttachmentSummary struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int    `json:"size_bytes"`
	ContentID   string `json:"content_id,omitempty"`
}

type Summary struct {
	From              string              `json:"from"`
	To                []string            `json:"to"`
	Cc                []string            `json:"cc"`
	Bcc               []string            `json:"bcc"`
	Subject           string              `json:"subject"`
	BodySummary       string              `json:"body_summary"`
	BodyPreview       string              `json:"body_preview,omitempty"`
	HTMLSourceExcerpt string              `json:"html_source_excerpt,omitempty"`
	HTMLBytes         int                 `json:"html_bytes,omitempty"`
	Attachments       []AttachmentSummary `json:"attachments"`
	RecipientCount    int                 `json:"recipient_count"`
	AllowlistReady    bool                `json:"allowlist_ready"`
	DeniedRecipients  []string            `json:"denied_recipients"`
}

func Build(draft Draft) ([]byte, error) {
	if err := validateDraft(draft); err != nil {
		return nil, err
	}
	if draft.Date.IsZero() {
		draft.Date = time.Now()
	}
	messageID, err := newMessageID(draft.From.Address)
	if err != nil {
		return nil, err
	}
	headers := []header{
		{"Date", draft.Date.Format(time.RFC1123Z)},
		{"Message-ID", messageID},
		{"From", draft.From.String()},
		{"To", addressList(draft.To)},
		{"Subject", mime.QEncoding.Encode("UTF-8", draft.Subject)},
		{"MIME-Version", "1.0"},
	}
	if len(draft.Cc) > 0 {
		headers = append(headers, header{"Cc", addressList(draft.Cc)})
	}
	if draft.InReplyTo != "" {
		headers = append(headers, header{"In-Reply-To", formatMessageID(draft.InReplyTo)})
	}
	if len(draft.References) > 0 {
		values := make([]string, 0, len(draft.References))
		for _, value := range draft.References {
			if strings.TrimSpace(value) != "" {
				values = append(values, formatMessageID(value))
			}
		}
		if len(values) > 0 {
			headers = append(headers, header{"References", strings.Join(values, " ")})
		}
	}
	var output bytes.Buffer
	if len(draft.Attachments) == 0 && len(draft.Inlines) == 0 && draft.BodyFormat != "html" {
		headers = append(headers, header{"Content-Type", `text/plain; charset="UTF-8"`}, header{"Content-Transfer-Encoding", "quoted-printable"})
		writeHeaders(&output, headers)
		writer := quotedprintable.NewWriter(&output)
		_, _ = writer.Write([]byte(normalizeCRLF(draft.Body)))
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	if len(draft.Attachments) == 0 && len(draft.Inlines) == 0 {
		// HTML body without attachments: the alternative pair is the whole
		// message body.
		multipartWriter := multipart.NewWriter(&output)
		headers = append(headers, header{"Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, multipartWriter.Boundary())})
		writeHeaders(&output, headers)
		if err := writeAlternativeBody(multipartWriter, draft.Body); err != nil {
			return nil, err
		}
		if err := multipartWriter.Close(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	if len(draft.Attachments) == 0 {
		// Inline images without regular attachments: the related container is
		// the whole message body — the alternative pair as its root part,
		// then one inline part per CID image.
		relatedWriter := multipart.NewWriter(&output)
		headers = append(headers, header{"Content-Type", fmt.Sprintf(`multipart/related; boundary="%s"; type="multipart/alternative"`, relatedWriter.Boundary())})
		writeHeaders(&output, headers)
		if err := writeAlternativePart(relatedWriter, draft.Body); err != nil {
			return nil, err
		}
		if err := writeInlineParts(relatedWriter, draft.Inlines); err != nil {
			return nil, err
		}
		if err := relatedWriter.Close(); err != nil {
			return nil, err
		}
		return output.Bytes(), nil
	}
	multipartWriter := multipart.NewWriter(&output)
	headers = append(headers, header{"Content-Type", fmt.Sprintf(`multipart/mixed; boundary="%s"`, multipartWriter.Boundary())})
	writeHeaders(&output, headers)
	switch {
	case len(draft.Inlines) > 0:
		// With both regular attachments and inline images the body part
		// becomes a multipart/related container (alternative pair plus one
		// part per inline image); the attachments follow at the mixed level.
		var related bytes.Buffer
		relatedWriter := multipart.NewWriter(&related)
		if err := writeAlternativePart(relatedWriter, draft.Body); err != nil {
			return nil, err
		}
		if err := writeInlineParts(relatedWriter, draft.Inlines); err != nil {
			return nil, err
		}
		if err := relatedWriter.Close(); err != nil {
			return nil, err
		}
		relatedHeader := textproto.MIMEHeader{}
		relatedHeader.Set("Content-Type", fmt.Sprintf(`multipart/related; boundary="%s"; type="multipart/alternative"`, relatedWriter.Boundary()))
		part, err := multipartWriter.CreatePart(relatedHeader)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(related.Bytes()); err != nil {
			return nil, err
		}
	case draft.BodyFormat == "html":
		// The alternative (text+html) pair replaces the bare text part;
		// the attachment loop below is unchanged.
		if err := writeAlternativePart(multipartWriter, draft.Body); err != nil {
			return nil, err
		}
	default:
		textHeader := textproto.MIMEHeader{}
		textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
		textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
		part, err := multipartWriter.CreatePart(textHeader)
		if err != nil {
			return nil, err
		}
		quoted := quotedprintable.NewWriter(part)
		_, _ = quoted.Write([]byte(normalizeCRLF(draft.Body)))
		if err := quoted.Close(); err != nil {
			return nil, err
		}
	}
	for i, attachment := range draft.Attachments {
		filename := safeio.SanitizeFilename(attachment.Filename, fmt.Sprintf("attachment-%d", i+1))
		contentType := attachment.ContentType
		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(filename))
		}
		contentType = safeContentType(contentType)
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", mime.FormatMediaType(contentType, map[string]string{"name": filename}))
		partHeader.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		partHeader.Set("Content-Transfer-Encoding", "base64")
		part, err := multipartWriter.CreatePart(partHeader)
		if err != nil {
			return nil, err
		}
		writeBase64Body(part, attachment.Data)
	}
	if err := multipartWriter.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// DerivePlainText renders a deterministic plain-text fallback for an HTML
// body: block tags become newlines, script/style blocks are dropped, tags
// are stripped, entities decoded, whitespace collapsed. It is what the
// recipient's text-mode client will roughly see and what the human confirms.
func DerivePlainText(source string) string {
	// Go's RE2 regexp has no backreferences, so the brief's single
	// `<(script|style)\b.*?</\1>` pattern becomes two literal-tag patterns
	// with identical (?is) semantics.
	s := regexp.MustCompile(`(?is)<script\b.*?</script\s*>`).ReplaceAllString(source, "")
	s = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6])\b[^>]*>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// writeAlternativeBody renders an HTML draft body as a multipart/alternative
// pair into w: a derived plain-text part (what text-mode clients roughly see)
// followed by the verbatim HTML part. The HTML is intentionally sent without
// any sanitization — the CLI is a transport, not a rewriter; the dry-run
// preview's escaped source excerpt is what the human reviews instead.
func writeAlternativeBody(w *multipart.Writer, body string) error {
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	textPart, err := w.CreatePart(textHeader)
	if err != nil {
		return err
	}
	quoted := quotedprintable.NewWriter(textPart)
	_, _ = quoted.Write([]byte(normalizeCRLF(DerivePlainText(body))))
	if err := quoted.Close(); err != nil {
		return err
	}
	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", `text/html; charset="UTF-8"`)
	htmlHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	htmlPart, err := w.CreatePart(htmlHeader)
	if err != nil {
		return err
	}
	quoted = quotedprintable.NewWriter(htmlPart)
	_, _ = quoted.Write([]byte(normalizeCRLF(body)))
	return quoted.Close()
}

// writeAlternativePart attaches the alternative (text+html) pair to root as a
// single nested multipart/alternative part — the body section shared by the
// mixed and related assemblies.
func writeAlternativePart(root *multipart.Writer, body string) error {
	var alternative bytes.Buffer
	alternativeWriter := multipart.NewWriter(&alternative)
	if err := writeAlternativeBody(alternativeWriter, body); err != nil {
		return err
	}
	if err := alternativeWriter.Close(); err != nil {
		return err
	}
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, alternativeWriter.Boundary()))
	part, err := root.CreatePart(partHeader)
	if err != nil {
		return err
	}
	_, err = part.Write(alternative.Bytes())
	return err
}

// writeInlineParts appends one part per inline image: sniffed content type, a
// Content-Id header (Build owns the angle-bracket wrapping so a bare or
// bracketed cid both end up correct), and an inline disposition using the same
// RFC 2231 filename encoding as regular attachments.
func writeInlineParts(w *multipart.Writer, inlines []Attachment) error {
	for i, inline := range inlines {
		filename := safeio.SanitizeFilename(inline.Filename, fmt.Sprintf("inline-%d", i+1))
		contentType := inline.ContentType
		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(filename))
		}
		contentType = safeContentType(contentType)
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", mime.FormatMediaType(contentType, map[string]string{"name": filename}))
		partHeader.Set("Content-Id", formatMessageID(inline.ContentID))
		partHeader.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
		partHeader.Set("Content-Transfer-Encoding", "base64")
		part, err := w.CreatePart(partHeader)
		if err != nil {
			return err
		}
		writeBase64Body(part, inline.Data)
	}
	return nil
}

// writeBase64Body writes data as base64 folded into 76-character lines — the
// shared transfer encoding for attachment and inline parts.
func writeBase64Body(part io.Writer, data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 76 {
		_, _ = fmt.Fprintf(part, "%s\r\n", encoded[:76])
		encoded = encoded[76:]
	}
	if encoded != "" {
		_, _ = fmt.Fprintf(part, "%s\r\n", encoded)
	}
}

func Summarize(draft Draft, allowlist []string) Summary {
	denied := DeniedRecipients(draft, allowlist)
	summary := Summary{From: draft.From.String(), To: addressStrings(draft.To), Cc: addressStrings(draft.Cc), Bcc: addressStrings(draft.Bcc), Subject: draft.Subject, BodySummary: truncateRunes(strings.TrimSpace(draft.Body), 240), Attachments: []AttachmentSummary{}, RecipientCount: len(draft.To) + len(draft.Cc) + len(draft.Bcc), AllowlistReady: len(allowlist) > 0 && len(denied) == 0, DeniedRecipients: denied}
	if draft.BodyFormat == "html" {
		summary.BodyPreview = DerivePlainText(draft.Body)
		summary.HTMLSourceExcerpt = htmlSourceExcerpt(draft.Body)
		summary.HTMLBytes = len(draft.Body)
	}
	for i, attachment := range draft.Attachments {
		filename := safeio.SanitizeFilename(attachment.Filename, fmt.Sprintf("attachment-%d", i+1))
		summary.Attachments = append(summary.Attachments, AttachmentSummary{Filename: filename, ContentType: attachment.ContentType, SizeBytes: len(attachment.Data)})
	}
	for i, inline := range draft.Inlines {
		filename := safeio.SanitizeFilename(inline.Filename, fmt.Sprintf("inline-%d", i+1))
		summary.Attachments = append(summary.Attachments, AttachmentSummary{Filename: filename, ContentType: inline.ContentType, SizeBytes: len(inline.Data), ContentID: inline.ContentID})
	}
	return summary
}

// htmlSourceExcerpt prepares the dry-run HTML review block: the raw source is
// escaped with the output package's markdown escaper (which also strips
// terminal controls and flattens line breaks) and capped at
// maxHTMLSourceExcerptBytes. HTMLBytes reports the full body size.
func htmlSourceExcerpt(body string) string {
	escaped := output.SanitizeMarkdown(body)
	if len(escaped) <= maxHTMLSourceExcerptBytes {
		return escaped
	}
	truncated := escaped[:maxHTMLSourceExcerptBytes]
	// Back off to a rune boundary so the cap never splits a character.
	for len(truncated) > 0 {
		if r, size := utf8.DecodeLastRuneInString(truncated); r != utf8.RuneError || size > 1 {
			break
		}
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

func DeniedRecipients(draft Draft, allowlist []string) []string {
	denied := []string{}
	for _, address := range append(append(append([]mail.Address{}, draft.To...), draft.Cc...), draft.Bcc...) {
		if !allowed(address.Address, allowlist) {
			denied = append(denied, strings.ToLower(address.Address))
		}
	}
	return denied
}

func Recipients(draft Draft) []string {
	result := []string{}
	for _, address := range append(append(append([]mail.Address{}, draft.To...), draft.Cc...), draft.Bcc...) {
		result = append(result, address.Address)
	}
	return result
}

type header struct{ name, value string }

func validateDraft(draft Draft) error {
	if strings.TrimSpace(draft.From.Address) == "" || len(draft.To)+len(draft.Cc)+len(draft.Bcc) == 0 {
		return fmt.Errorf("from and at least one recipient are required")
	}
	if len(draft.To)+len(draft.Cc)+len(draft.Bcc) > MaxRecipientsPerMessage {
		return fmt.Errorf("recipient count exceeds %d", MaxRecipientsPerMessage)
	}
	switch draft.BodyFormat {
	case "", "text", "html":
	default:
		return fmt.Errorf("unsupported body format %q", draft.BodyFormat)
	}
	if len(draft.Body) > MaxBodyBytes {
		return fmt.Errorf("body exceeds %d bytes", MaxBodyBytes)
	}
	for _, value := range []string{draft.Subject, draft.InReplyTo, strings.Join(draft.References, " "), draft.From.Name, draft.From.Address} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("header contains a line break")
		}
	}
	for _, address := range append(append(append([]mail.Address{}, draft.To...), draft.Cc...), draft.Bcc...) {
		if strings.ContainsAny(address.Name+address.Address, "\r\n") {
			return fmt.Errorf("address contains a line break")
		}
	}
	var total int64
	for _, attachment := range draft.Attachments {
		total += int64(len(attachment.Data))
	}
	for _, inline := range draft.Inlines {
		total += int64(len(inline.Data))
	}
	if total > MaxTotalAttachmentBytes {
		return fmt.Errorf("attachments exceed %d bytes", MaxTotalAttachmentBytes)
	}
	if len(draft.Inlines) > 0 {
		if draft.BodyFormat != "html" {
			return fmt.Errorf("内嵌附件需要 --body-format html")
		}
		for _, inline := range draft.Inlines {
			if strings.ContainsAny(inline.Filename, "\r\n") || strings.ContainsAny(inline.ContentID, "\r\n") {
				return fmt.Errorf("inline attachment header contains a line break")
			}
			if strings.ContainsFunc(inline.ContentID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				return fmt.Errorf("inline attachment content id contains a control character")
			}
			if strings.TrimSpace(inline.ContentID) == "" {
				return fmt.Errorf("inline attachment requires a content id")
			}
		}
	}
	return nil
}

func safeContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(mediaType, "/") || strings.ContainsAny(mediaType, "\r\n") {
		return "application/octet-stream"
	}
	return mediaType
}

func writeHeaders(output *bytes.Buffer, headers []header) {
	for _, header := range headers {
		if header.value != "" {
			_, _ = fmt.Fprintf(output, "%s: %s\r\n", header.name, header.value)
		}
	}
	output.WriteString("\r\n")
}

func addressList(values []mail.Address) string { return strings.Join(addressStrings(values), ", ") }
func addressStrings(values []mail.Address) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].String()
	}
	return result
}

func allowed(address string, allowlist []string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == address {
			return true
		}
		if strings.HasPrefix(entry, "*@") && strings.HasSuffix(address, entry[1:]) && strings.Count(address, "@") == 1 {
			return true
		}
	}
	return false
}

func newMessageID(address string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	domain := "localhost"
	if at := strings.LastIndexByte(address, '@'); at >= 0 && at < len(address)-1 {
		domain = address[at+1:]
	}
	return fmt.Sprintf("<%x@%s>", raw, domain), nil
}

// NewContentID returns a fresh bare Content-ID ("hex@qqmail-cli.local", no
// angle brackets — Build wraps it when writing the Content-Id header). Like
// newMessageID it returns the rand failure instead of panicking.
func NewContentID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s@qqmail-cli.local", hex.EncodeToString(b[:])), nil
}

func formatMessageID(value string) string {
	return "<" + strings.Trim(strings.TrimSpace(value), "<>") + ">"
}

func normalizeCRLF(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "…"
}

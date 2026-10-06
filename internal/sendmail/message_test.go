package sendmail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
)

func TestBuildEncodesChineseHeadersAndAttachmentName(t *testing.T) {
	draft := Draft{
		From:    mail.Address{Name: "发件人", Address: "sender@qq.com"},
		To:      []mail.Address{{Name: "收件人", Address: "reader@example.com"}},
		Bcc:     []mail.Address{{Address: "hidden@example.com"}},
		Subject: "中文主题", Body: "你好，世界",
		InReplyTo: "original@example.com", References: []string{"older@example.com", "original@example.com"},
		Attachments: []Attachment{{Filename: "测试报告.pdf", ContentType: "application/pdf", Data: []byte("fixture")}},
	}
	raw, err := Build(draft)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("bcc:")) || bytes.Contains(raw, []byte("hidden@example.com")) {
		t.Fatalf("Bcc leaked into MIME headers: %s", raw)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil || subject != "中文主题" {
		t.Fatalf("subject=%q err=%v", subject, err)
	}
	if message.Header.Get("In-Reply-To") != "<original@example.com>" || !strings.Contains(message.Header.Get("References"), "<older@example.com>") {
		t.Fatalf("thread headers missing: %#v", message.Header)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("content type=%q params=%v err=%v", mediaType, params, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	_, err = reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	_, disposition, err := mime.ParseMediaType(attachment.Header.Get("Content-Disposition"))
	if err != nil || disposition["filename"] != "测试报告.pdf" {
		t.Fatalf("filename params=%v err=%v", disposition, err)
	}
	data, err := io.ReadAll(attachment)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		t.Fatalf("attachment body missing: %q err=%v", data, err)
	}
}

func TestRecipientAllowlistRequiresEveryRecipient(t *testing.T) {
	draft := Draft{To: []mail.Address{{Address: "one@example.com"}}, Cc: []mail.Address{{Address: "two@trusted.test"}}}
	if denied := DeniedRecipients(draft, []string{"one@example.com", "*@trusted.test"}); len(denied) != 0 {
		t.Fatalf("unexpected denied recipients: %v", denied)
	}
	if denied := DeniedRecipients(draft, nil); len(denied) != 2 {
		t.Fatalf("empty allowlist did not deny all recipients: %v", denied)
	}
	// Partial hit: one allowed recipient must not carry a stranger through,
	// and the denial must name exactly the stranger.
	partial := Draft{To: []mail.Address{{Address: "one@example.com"}}, Cc: []mail.Address{{Address: "stranger@evil.test"}}}
	denied := DeniedRecipients(partial, []string{"one@example.com"})
	if len(denied) != 1 || denied[0] != "stranger@evil.test" {
		t.Fatalf("partial allowlist hit mishandled: %v", denied)
	}
	// Bcc participates in the check like every other recipient.
	hidden := Draft{To: []mail.Address{{Address: "one@example.com"}}, Bcc: []mail.Address{{Address: "sneak@evil.test"}}}
	denied = DeniedRecipients(hidden, []string{"one@example.com"})
	if len(denied) != 1 || denied[0] != "sneak@evil.test" {
		t.Fatalf("bcc escaped the allowlist: %v", denied)
	}
}

func TestHeaderInjectionRejected(t *testing.T) {
	_, err := Build(Draft{From: mail.Address{Address: "a@example.com"}, To: []mail.Address{{Address: "b@example.com"}}, Subject: "ok\r\nBcc: bad@example.com"})
	if err == nil {
		t.Fatal("expected header injection rejection")
	}
}

func TestBuildHTMLOnlyProducesAlternative(t *testing.T) {
	raw, err := Build(Draft{
		From:    mail.Address{Address: "sender@qq.com"},
		To:      []mail.Address{{Address: "reader@example.com"}},
		Subject: "s",
		Body:    "<p>你好 <b>世界</b></p>", BodyFormat: "html",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// go-message has no TextPart/HTMLPart helper, so the assertion walks the
	// entity tree and matches parts by Content-Type.
	text, html := extractAltParts(t, raw)
	if text != "你好 世界" {
		t.Fatalf("derived text = %q", text)
	}
	if !strings.Contains(html, "<p>你好 <b>世界</b></p>") {
		t.Fatalf("html part altered: %q", html)
	}
}

func TestBuildHTMLWithAttachmentsNestsAlternative(t *testing.T) {
	raw, err := Build(Draft{
		From: mail.Address{Address: "sender@qq.com"}, To: []mail.Address{{Address: "reader@example.com"}}, Subject: "s",
		Body: "<p>你好 <b>世界</b></p>", BodyFormat: "html",
		Attachments: []Attachment{{Filename: "报告.pdf", ContentType: "application/pdf", Data: []byte("fixture")}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("root content type=%q err=%v", mediaType, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	first, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	firstType, _, err := mime.ParseMediaType(first.Header.Get("Content-Type"))
	if err != nil || firstType != "multipart/alternative" {
		t.Fatalf("mixed[0] = %q err=%v, want multipart/alternative", firstType, err)
	}
	second, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	_, disposition, err := mime.ParseMediaType(second.Header.Get("Content-Disposition"))
	if err != nil || disposition["filename"] != "报告.pdf" {
		t.Fatalf("mixed[1] should be the attachment: %v err=%v", disposition, err)
	}
	// The nested alternative must still carry the text+html pair.
	text, html := extractAltParts(t, raw)
	if text != "你好 世界" {
		t.Fatalf("derived text = %q", text)
	}
	if !strings.Contains(html, "<p>你好 <b>世界</b></p>") {
		t.Fatalf("html part altered: %q", html)
	}
}

// extractAltParts locates the message's multipart/alternative body and returns
// its decoded text/plain and text/html leaf parts. go-message has no
// TextPart/HTMLPart helper, so the tree is walked with entity.Walk and parts
// are matched by Content-Type; the parts must be children of the alternative.
func extractAltParts(t *testing.T, raw []byte) (text, html string) {
	t.Helper()
	entity, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("MIME parse: %v", err)
	}
	type leafPart struct {
		path      []int
		mediaType string
		body      string
	}
	type multiPart struct {
		path      []int
		mediaType string
	}
	var multiparts []multiPart
	var leaves []leafPart
	err = entity.Walk(func(path []int, part *gomessage.Entity, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		mediaType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(mediaType, "multipart/") {
			multiparts = append(multiparts, multiPart{path: path, mediaType: mediaType})
			return nil
		}
		content, readErr := io.ReadAll(part.Body)
		if readErr != nil {
			return readErr
		}
		leaves = append(leaves, leafPart{path: path, mediaType: mediaType, body: string(content)})
		return nil
	})
	if err != nil {
		t.Fatalf("entity walk: %v", err)
	}
	var altPath []int
	found := false
	for _, mp := range multiparts {
		if mp.mediaType == "multipart/alternative" {
			altPath, found = mp.path, true
			break
		}
	}
	if !found {
		t.Fatalf("no multipart/alternative in message; multiparts=%v leaves=%v", multiparts, leaves)
	}
	isChild := func(p []int) bool {
		if len(p) != len(altPath)+1 {
			return false
		}
		for i, index := range altPath {
			if p[i] != index {
				return false
			}
		}
		return true
	}
	for _, leaf := range leaves {
		if !isChild(leaf.path) {
			continue
		}
		switch leaf.mediaType {
		case "text/plain":
			text = leaf.body
		case "text/html":
			html = leaf.body
		}
	}
	if text == "" || html == "" {
		t.Fatalf("alternative missing its text/html pair: text=%q html=%q leaves=%v", text, html, leaves)
	}
	return text, html
}

// DerivePlainText is deterministic: tags stripped, script/style dropped,
// entities decoded, all whitespace collapsed. The whitespace collapse means
// the fallback is a single line even for multi-block HTML.
func TestDerivePlainText(t *testing.T) {
	got := DerivePlainText("<html><head><style>p { color: red }</style></head><body><p>你好 <b>世界</b></p><br><p>A &amp; B</p><script>alert(1)</script></body></html>")
	if want := "你好 世界 A & B"; got != want {
		t.Fatalf("DerivePlainText = %q, want %q", got, want)
	}
}

// The 2048-byte excerpt cap must not split an escaped entity: SanitizeMarkdown
// writes every '&' as a complete &#NN; escape, so a cut landing inside one
// leaves a half-written escape at the tail — the excerpt backs off to that
// '&' instead.
func TestHTMLSourceExcerptNeverSplitsEscapedEntity(t *testing.T) {
	// "<x>" repeats escape to 11-byte "&#60;x&#62;" runs; 2048 mod 11 = 2, so
	// the plain cut lands two bytes into an entity ("&#").
	excerpt := htmlSourceExcerpt(strings.Repeat("<x>", 700))
	if len(excerpt) == 0 || len(excerpt) > maxHTMLSourceExcerptBytes {
		t.Fatalf("excerpt length %d out of bounds", len(excerpt))
	}
	if strings.Count(excerpt, "&") != strings.Count(excerpt, ";") {
		t.Fatalf("excerpt contains an unterminated escape near %q", excerpt[max(0, len(excerpt)-40):])
	}
	if !strings.HasSuffix(excerpt, ";") {
		t.Fatalf("excerpt ends mid-escape: %q", excerpt[max(0, len(excerpt)-40):])
	}
}

// Package-level send/receive fixtures shared by the inline-image tests.
var (
	from = mail.Address{Address: "sender@qq.com"}
	to   = mail.Address{Address: "reader@example.com"}
)

// pngBytes is the PNG magic signature; Build treats inline data as opaque, so
// the signature is all a build-path test needs.
var pngBytes = []byte("\x89PNG\r\n\x1a\n")

// relatedParts mirrors the multipart/related node of a parsed message together
// with its direct children (go-message has no Parts accessor, so the test
// materializes them from the walk paths).
type relatedParts struct {
	root  *gomessage.Entity
	parts []*gomessage.Entity
}

// findRelatedPart parses raw and returns the first multipart/related entity
// with its direct children, in wire order.
func findRelatedPart(t *testing.T, raw []byte) (*relatedParts, bool) {
	t.Helper()
	entity, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("MIME parse: %v", err)
	}
	type node struct {
		path   []int
		entity *gomessage.Entity
	}
	var nodes []node
	if err := entity.Walk(func(path []int, part *gomessage.Entity, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		nodes = append(nodes, node{path: path, entity: part})
		return nil
	}); err != nil {
		t.Fatalf("entity walk: %v", err)
	}
	var relatedPath []int
	result := &relatedParts{}
	found := false
	for _, n := range nodes {
		mediaType, _, _ := mime.ParseMediaType(n.entity.Header.Get("Content-Type"))
		if mediaType == "multipart/related" {
			relatedPath, found = n.path, true
			result.root = n.entity
			break
		}
	}
	if !found {
		return nil, false
	}
	isDirectChild := func(p []int) bool {
		if len(p) != len(relatedPath)+1 {
			return false
		}
		for i, index := range relatedPath {
			if p[i] != index {
				return false
			}
		}
		return true
	}
	for _, n := range nodes {
		if isDirectChild(n.path) {
			result.parts = append(result.parts, n.entity)
		}
	}
	return result, true
}

// findAlternativeIn reports whether entity is (or contains) a
// multipart/alternative part.
func findAlternativeIn(t *testing.T, entity *gomessage.Entity) (*gomessage.Entity, bool) {
	t.Helper()
	if mediaType, _, _ := mime.ParseMediaType(entity.Header.Get("Content-Type")); mediaType == "multipart/alternative" {
		return entity, true
	}
	var alternative *gomessage.Entity
	if err := entity.Walk(func(path []int, part *gomessage.Entity, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if part == entity {
			return nil
		}
		if mediaType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type")); mediaType == "multipart/alternative" && alternative == nil {
			alternative = part
		}
		return nil
	}); err != nil {
		t.Fatalf("entity walk: %v", err)
	}
	return alternative, alternative != nil
}

func TestBuildInlineImagesProducesRelated(t *testing.T) {
	raw, err := Build(Draft{From: from, To: []mail.Address{to}, Subject: "s",
		Body: `<p>见图 <img src="cid:image001"></p>`, BodyFormat: "html",
		Inlines: []Attachment{{Filename: "logo.png", ContentType: "image/png", ContentID: "image001", Data: []byte(pngBytes)}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rel, ok := findRelatedPart(t, raw)
	if !ok {
		t.Fatal("want multipart/related")
	}
	if _, ok := findAlternativeIn(t, rel.parts[0]); !ok {
		t.Fatal("related root must be the alternative")
	}
	if len(rel.parts) != 2 {
		t.Fatalf("related has %d parts, want 2 (alternative + image)", len(rel.parts))
	}
	img := rel.parts[1]
	if img.Header.Get("Content-Id") != "<image001>" { // Build 侧负责尖括号包裹
		t.Fatalf("Content-Id = %q", img.Header.Get("Content-Id"))
	}
	if !strings.Contains(img.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("disposition = %q", img.Header.Get("Content-Disposition"))
	}
}

func TestBuildInlineRequiresHTMLBody(t *testing.T) {
	_, err := Build(Draft{From: from, To: []mail.Address{to}, Subject: "s",
		Inlines: []Attachment{{Filename: "a.png", ContentID: "x", Data: []byte("p")}}})
	if err == nil || !strings.Contains(err.Error(), "html") {
		t.Fatalf("want error: inline attachments require an html body, got %v", err)
	}
}

func TestInlineHeaderInjectionRejected(t *testing.T) {
	// Filename 或 ContentID 含 \r/\n → Build 返回错误（既有头部注入防御扩展）
	for _, tc := range []struct {
		name    string
		inlines []Attachment
	}{
		{"crlf in filename", []Attachment{{Filename: "a\r\nBcc: bad@example.com", ContentID: "x", Data: []byte("p")}}},
		{"crlf in content id", []Attachment{{Filename: "a.png", ContentID: "x\r\nBcc: bad@example.com", Data: []byte("p")}}},
	} {
		draft := Draft{From: from, To: []mail.Address{to}, Subject: "s", Body: "<p>x</p>", BodyFormat: "html", Inlines: tc.inlines}
		if _, err := Build(draft); err == nil {
			t.Fatalf("%s: expected inline header injection rejection", tc.name)
		}
	}
}

// The two validateDraft Content-ID defenses get direct assertions (review
// Minor #1): a control character in the id — the kind of byte a raw file name
// carries into a hand-built Draft — and a whitespace-only id that would
// otherwise be written as the empty header "Content-Id: <>".
func TestInlineContentIDValidationBranches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		contentID string
		wantErr   string
	}{
		{"control character in content id", "logo\x01.png@qqmail-cli.local", "control character"},
		{"whitespace-only content id", "   ", "content id"},
	} {
		draft := Draft{From: from, To: []mail.Address{to}, Subject: "s", Body: "<p>x</p>", BodyFormat: "html",
			Inlines: []Attachment{{Filename: "logo.png", ContentID: tc.contentID, Data: []byte(pngBytes)}}}
		_, err := Build(draft)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestBuildRejectsUnknownBodyFormat(t *testing.T) {
	_, err := Build(Draft{From: mail.Address{Address: "a@example.com"}, To: []mail.Address{{Address: "b@example.com"}}, Body: "hi", BodyFormat: "markdown"})
	if err == nil {
		t.Fatal("unknown body format was accepted")
	}
}

func TestBuildEnforcesOneMiBBodyCapForBothFormats(t *testing.T) {
	for _, format := range []string{"", "text", "html"} {
		oversize := Draft{From: mail.Address{Address: "a@example.com"}, To: []mail.Address{{Address: "b@example.com"}}, Body: strings.Repeat("a", MaxBodyBytes+1), BodyFormat: format}
		if _, err := Build(oversize); err == nil {
			t.Fatalf("format %q: oversize body accepted", format)
		}
	}
	atLimit := Draft{From: mail.Address{Address: "a@example.com"}, To: []mail.Address{{Address: "b@example.com"}}, Body: strings.Repeat("a", MaxBodyBytes), BodyFormat: "html"}
	if _, err := Build(atLimit); err != nil {
		t.Fatalf("body at the 1 MiB limit was rejected: %v", err)
	}
}

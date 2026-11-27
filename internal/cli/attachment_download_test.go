package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/situker/qqmail-cli/internal/account"
	"github.com/situker/qqmail-cli/internal/errmap"
	"github.com/situker/qqmail-cli/internal/imapx"
	"github.com/situker/qqmail-cli/internal/mailmodel"
	"github.com/situker/qqmail-cli/internal/secrets"
)

type attachmentFixtureReader struct {
	fakeReader
	raw []byte
}

func (r attachmentFixtureReader) FetchMessage(context.Context, mailmodel.MsgID) ([]byte, error) {
	return r.raw, nil
}

func TestAttachmentDownloadImageExtension(t *testing.T) {
	t.Setenv("QQMAIL_CLI_READONLY", "0")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := &account.Config{
		Schema: account.ConfigSchema, DefaultAccount: "personal",
		Accounts: map[string]account.Account{"personal": {Email: "reader@example.com"}},
	}
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	id := mailmodel.MsgID{Folder: "INBOX", UIDValidity: 1, UID: 1}.String()

	pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
	pixel.SetRGBA(0, 0, color.RGBA{R: 160, G: 40, B: 80, A: 255})
	encodeImage := func(encode func(io.Writer, image.Image) error) string {
		t.Helper()
		var buf bytes.Buffer
		if err := encode(&buf, pixel); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	jpegBytes := encodeImage(func(w io.Writer, im image.Image) error { return jpeg.Encode(w, im, nil) })
	pngBytes := encodeImage(png.Encode)
	gifBytes := encodeImage(func(w io.Writer, im image.Image) error { return gif.Encode(w, im, nil) })
	const webp = "RIFF\x00\x00\x00\x00WEBPVP8 fixture"
	longStem := strings.Repeat("图", 53)
	parts := []struct {
		name, mediaType, data, want, decodeFormat string
	}{
		{"opaque.123", "application/octet-stream", jpegBytes, "opaque.123.jpg", "jpeg"},
		{"already.JpEg", "application/octet-stream", jpegBytes, "already.JpEg", "jpeg"},
		{"poster.jpg", "image/jpeg", pngBytes, "poster.jpg.png", "png"},
		{"animation", "application/octet-stream", gifBytes, "animation.gif", "gif"},
		{"picture.WeBp", "application/octet-stream", webp, "picture.WeBp", ""},
		{"web-image", "application/octet-stream", webp, "web-image.webp", ""},
		{"unknown.bin", "image/png", "not image bytes", "unknown.bin", ""},
		{longStem + ".JPEG", "application/octet-stream", jpegBytes, strings.Repeat("图", 48) + ".JPEG", "jpeg"},
		{longStem, "application/octet-stream", pngBytes, strings.Repeat("图", 48) + ".png", "png"},
		{"CON .PNG", "application/octet-stream", pngBytes, "attachment-10.PNG", "png"},
	}

	var fixture strings.Builder
	fixture.WriteString("From: fixture@example.net\r\nSubject: Images\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=fixture\r\n\r\n" +
		"--fixture\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:fixture\">\r\n")
	for i, part := range parts {
		disposition := "attachment"
		if i == 0 {
			disposition = "inline"
		}
		fmt.Fprintf(&fixture, "--fixture\r\nContent-Type: %s\r\nContent-Disposition: %s\r\n"+
			"Content-ID: <fixture-%d@example.net>\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			part.mediaType, mime.FormatMediaType(disposition, map[string]string{"filename": part.name}),
			i, base64.StdEncoding.EncodeToString([]byte(part.data)))
	}
	fixture.WriteString("--fixture--\r\n")

	runDownload := func(selector, outputDir string, flags ...string) ([]byte, error) {
		var out, stderr bytes.Buffer
		rt := &Runtime{
			Out: &out, Err: &stderr, In: strings.NewReader(""),
			Secrets: &secrets.Memory{Values: map[string]string{"reader@example.com": "abcdefghijklmnop"}},
			Dial: func(context.Context, account.Named, string) (imapx.Reader, error) {
				return attachmentFixtureReader{raw: []byte(fixture.String())}, nil
			},
		}
		root := NewRoot(rt)
		root.SetArgs(append([]string{"--config", configPath, "--json", "attachment", "download",
			id, selector, "--output", outputDir}, flags...))
		err := root.Execute()
		return out.Bytes(), err
	}

	parseDownloaded := func(data []byte) []string {
		t.Helper()
		validateOutput(t, "attachment.download.schema.json", data)
		var result struct {
			Data struct {
				Downloaded []struct {
					Path string
				}
			}
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, len(result.Data.Downloaded))
		for _, item := range result.Data.Downloaded {
			paths = append(paths, item.Path)
		}
		return paths
	}

	outputDir := filepath.Join(dir, "下载 图片")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existingPath := filepath.Join(outputDir, parts[0].want)
	if err := os.WriteFile(existingPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := runDownload("all", outputDir, "--image-extension")
	if err != nil {
		t.Fatal(err)
	}
	paths := parseDownloaded(data)
	if len(paths) != len(parts) {
		t.Fatalf("downloaded %d parts, want %d", len(paths), len(parts))
	}
	for i, part := range parts {
		want := part.want
		if i == 0 {
			want = "opaque.123 (1).jpg"
		}
		if paths[i] != filepath.Join(outputDir, want) {
			t.Errorf("part %d: path %q, want %q in %q", i+1, paths[i], want, outputDir)
		}
		raw, err := os.ReadFile(paths[i])
		if err != nil {
			t.Errorf("part %d: read downloaded file: %v", i+1, err)
			continue
		}
		if string(raw) != part.data {
			t.Errorf("part %d: download changed the attachment bytes", i+1)
		}
		if part.decodeFormat != "" {
			decoded, format, err := image.Decode(bytes.NewReader(raw))
			if err != nil || format != part.decodeFormat {
				t.Errorf("part %d: expected a decodable %s image, got format=%q err=%v", i+1, part.decodeFormat, format, err)
			} else if decoded.Bounds() != pixel.Bounds() {
				t.Errorf("part %d: image bounds %v, want %v", i+1, decoded.Bounds(), pixel.Bounds())
			}
		}
	}
	if raw, err := os.ReadFile(existingPath); err != nil || string(raw) != "existing" {
		t.Fatalf("existing file changed: contents=%q, err=%v", raw, err)
	}

	legacyDir := filepath.Join(dir, "默认命名")
	data, err = runDownload("1", legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if paths := parseDownloaded(data); len(paths) != 1 || paths[0] != filepath.Join(legacyDir, parts[0].name) {
		t.Fatalf("default naming changed: %v", paths)
	}

	limitedDir := filepath.Join(dir, "限额")
	if _, err := runDownload("1", limitedDir, "--image-extension", "--max-size", "1"); err == nil || errmap.Classify(err).Kind != errmap.PolicyDenied {
		t.Fatalf("image extension bypassed --max-size: %v", err)
	}
	if _, err := os.Stat(limitedDir); !os.IsNotExist(err) {
		t.Fatalf("oversized attachment created output directory, stat error=%v", err)
	}
}

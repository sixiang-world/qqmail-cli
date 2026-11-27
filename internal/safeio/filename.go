package safeio

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxFilenameBytes = 150

var windowsReserved = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?$`)

func SanitizeFilename(name string, fallback string) string {
	name = truncateUTF8(sanitizeFilename(name, fallback), maxFilenameBytes)
	if name == "" {
		return fallback
	}
	return name
}

// SanitizeFilenameWithExtension appends a trusted extension or retains a
// matching extension, reserving space for it within the filename length limit.
// The extension and its aliases must include their leading dot.
func SanitizeFilenameWithExtension(name, fallback, extension string, aliases ...string) string {
	name = sanitizeFilename(name, fallback)
	existing := filepath.Ext(name)
	if strings.EqualFold(existing, extension) || slices.ContainsFunc(aliases, func(alias string) bool {
		return strings.EqualFold(existing, alias)
	}) {
		name = strings.TrimSuffix(name, existing)
		extension = existing
	}
	name = strings.TrimRight(truncateUTF8(name, maxFilenameBytes-len(extension)), " .")
	if name == "" || windowsReserved.MatchString(name) {
		name = fallback
	}
	return name + extension
}

func sanitizeFilename(name, fallback string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, name)
	name = strings.ReplaceAll(name, "..", "")
	name = strings.Trim(name, " .")
	if name == "" || windowsReserved.MatchString(name) {
		return fallback
	}
	return name
}

func UniquePath(dir, filename string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	filename = SanitizeFilename(filename, "attachment")
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 0; ; i++ {
		candidateName := filename
		if i > 0 {
			candidateName = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		candidate := filepath.Join(root, candidateName)
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(root, absolute)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe output path")
		}
		if _, err := os.Stat(absolute); os.IsNotExist(err) {
			return absolute, nil
		} else if err != nil {
			return "", err
		}
	}
}

func truncateUTF8(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

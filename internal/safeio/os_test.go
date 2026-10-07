package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

// osWriteFile is the test-side shorthand used by the filename fixtures.
var osWriteFile = os.WriteFile

// WriteFileAtomic must replace existing content via rename and leave no temp
// files behind — the daily autosend counter depends on both.
func TestWriteFileAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFileAtomic(path, []byte(`{"count":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte(`{"count":2}`), 0o600); err != nil {
		t.Fatalf("second atomic write must replace: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != `{"count":2}` {
		t.Fatalf("content=%q err=%v", raw, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestWriteFileAtomicCreatesMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "x" {
		t.Fatalf("content=%q err=%v", raw, err)
	}
}

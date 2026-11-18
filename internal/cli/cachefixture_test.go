package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/situker/qqmail-cli/internal/index"
)

// On Windows the SQLite driver's last-close checkpoint can release its file
// handles a moment after the test's deferred store.Close() has returned. The
// testing framework's immediate TempDir RemoveAll then races that window and
// intermittently fails cleanup with "The directory is not empty" — any test
// holding an index cache can hit it under load (observed on main as well).
// The retried removal registers after t.TempDir(), so by LIFO it runs before
// the framework's own cleanup and absorbs the transient handle window.
func newTestCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if err := os.RemoveAll(dir); err == nil || time.Now().After(deadline) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return dir
}

// openTestCache opens a throwaway index DB with the flake-resistant cleanup.
func openTestCache(t *testing.T, write bool) (*index.DB, error) {
	t.Helper()
	return index.OpenPath(filepath.Join(newTestCacheDir(t), "cache.db"), write)
}

// testCachePath returns a cache.db path whose directory gets the
// flake-resistant cleanup, for runtimes that open the index through a closure.
func testCachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(newTestCacheDir(t), "cache.db")
}

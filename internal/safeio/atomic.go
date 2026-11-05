package safeio

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path through a temp file in the same
// directory followed by an atomic rename, so a crash mid-write can never
// leave a truncated file behind (the same pattern the config and cleanup-plan
// savers use). The parent directory is created if missing.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// The file itself was fsync'd above. On POSIX, syncing the containing
	// directory also makes the renamed directory entry crash-durable.
	return syncParentDir(dir)
}

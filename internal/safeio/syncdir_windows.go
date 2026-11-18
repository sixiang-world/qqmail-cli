//go:build windows

package safeio

// Windows cannot portably fsync a directory using os.File.Sync.
// The state file is flushed before replacement; callers must not claim
// power-loss durability of the directory entry on Windows.
func syncParentDir(string) error { return nil }

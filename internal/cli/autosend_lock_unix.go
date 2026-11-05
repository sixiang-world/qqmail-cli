//go:build !windows

package cli

import (
    "os"
    "path/filepath"

    "golang.org/x/sys/unix"
)

func lockAutoSend(path string) (func() error, error) {
    if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { return nil, err }
    f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
    if err != nil { return nil, err }
    if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
        _ = f.Close()
        return nil, err
    }
    return func() error {
        err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
        closeErr := f.Close()
        if err != nil { return err }
        return closeErr
    }, nil
}

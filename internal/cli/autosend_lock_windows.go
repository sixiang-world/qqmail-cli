//go:build windows

package cli

import (
    "os"
    "path/filepath"

    "golang.org/x/sys/windows"
)

func lockAutoSend(path string) (func() error, error) {
    if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { return nil, err }
    f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
    if err != nil { return nil, err }
    var overlap windows.Overlapped
    if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlap); err != nil {
        _ = f.Close()
        return nil, err
    }
    return func() error {
        err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlap)
        closeErr := f.Close()
        if err != nil { return err }
        return closeErr
    }, nil
}

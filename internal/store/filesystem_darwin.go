//go:build darwin

package store

import (
	"errors"
	"syscall"
)

// databaseFilesystem approximates the Linux policy on macOS: only local
// (non-network) mounts are accepted. Darwin exposes a MNT_LOCAL flag instead
// of filesystem magic, so BeeGFS and journal-mode combinations are not
// separately detected here; deployment targets Linux.
func databaseFilesystem(path, journal string, allowUnverified bool) (bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false, err
	}
	if stat.Flags&0x1000 == 0 {
		return false, errors.New("the application database requires a local filesystem")
	}
	return false, nil
}

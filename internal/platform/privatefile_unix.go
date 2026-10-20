//go:build !windows

package platform

import "os"

// CreatePrivateFile creates a new owner-only file and refuses any existing
// filesystem entry, including symlinks. The caller owns the returned handle.
func CreatePrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

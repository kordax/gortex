package mcp

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/platform"
)

// durableAtomicRestoreSymlink restores the directory entry without following
// its target. The coordinator syncs the parent directory after rollback.
func durableAtomicRestoreSymlink(path, target string) error {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".gortex-batch-link-*")
	if err != nil {
		return fmt.Errorf("create temporary symlink directory: %w", err)
	}
	defer os.RemoveAll(dir)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("create temporary symlink: %w", err)
	}
	if err := platform.ReplaceFile(link, path); err != nil {
		return fmt.Errorf("replace symlink entry: %w", err)
	}
	return nil
}

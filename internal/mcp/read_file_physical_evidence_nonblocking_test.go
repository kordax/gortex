//go:build !windows

package mcp

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A bare FIFO is rejected by the pre-open Lstat and never reaches the open, so
// it cannot pin the non-blocking open flag. A symlink to a FIFO does: Lstat
// reports a symlink, the mode check passes it through, and the open is what
// has to return instead of waiting for a writer. Drop O_NONBLOCK from
// openPhysicalEvidenceFile and this test hangs until the deadline.
func TestReadFilePhysicalEvidenceSymlinkToFIFOOpensWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "target.fifo")
	link := filepath.Join(dir, "evidence.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	require.NoError(t, os.Symlink(fifo, link))

	done := make(chan error, 1)
	go func() {
		_, _, err := readPhysicalFileEvidence(link)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires a regular file")
	case <-time.After(5 * time.Second):
		// Unblock the vulnerable implementation before failing so the test does
		// not leak a goroutine or leave cleanup waiting on an open FIFO.
		if writer, oerr := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); oerr == nil {
			_ = writer.Close()
		}
		err := <-done
		t.Fatalf("physical evidence blocked while opening a symlink to a FIFO: %v", err)
	}
}

// The remaining special-file shapes must all fail closed rather than serve a
// digest for something that is not a regular file.
func TestReadFilePhysicalEvidenceRejectsSpecialFiles(t *testing.T) {
	dir := t.TempDir()

	// A unix socket path is capped near 104 bytes on macOS, well under the
	// length of a t.TempDir() name, so the socket gets its own short root.
	socketRoot, err := os.MkdirTemp("", "gxev")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socket := filepath.Join(socketRoot, "evidence.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	subdir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	linkedDir := filepath.Join(dir, "linked-dir")
	require.NoError(t, os.Symlink(subdir, linkedDir))

	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "no-such-file"), dangling))

	for name, path := range map[string]string{
		"socket":           socket,
		"directory":        subdir,
		"symlink_to_dir":   linkedDir,
		"dangling_symlink": dangling,
		"missing":          filepath.Join(dir, "absent.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, rerr := readPhysicalFileEvidence(path)
			require.Error(t, rerr, "physical evidence must fail closed for %s", name)
		})
	}
}

//go:build !windows

package mcp

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadFilePhysicalEvidenceRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	done := make(chan error, 1)
	go func() {
		_, _, err := readPhysicalFileEvidence(path)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires a regular file")
	case <-time.After(500 * time.Millisecond):
		// Unblock the vulnerable implementation before failing so the test does
		// not leak a goroutine or leave cleanup waiting on an open FIFO.
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = writer.Close()
		}
		readErr := <-done
		t.Fatalf("physical evidence blocked while opening a FIFO: %v", readErr)
	}
}

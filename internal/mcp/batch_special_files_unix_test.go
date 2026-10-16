//go:build unix

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A failing implementation must release its blocked reader before the test
// exits, so the regression itself never leaves a worker or path lock behind.
func awaitBatchFIFORefusal(t *testing.T, fifo string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		return
	case <-time.After(time.Second):
		t.Error("batch blocked while reading a nonregular file")
	}
	fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
	require.NoError(t, err, "release the stranded FIFO reader")
	_, writeErr := unix.Write(fd, []byte("fixture\n"))
	closeErr := unix.Close(fd)
	require.NoError(t, writeErr)
	require.NoError(t, closeErr)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not finish after its FIFO reader was released")
	}
}

func TestBatchRefusesNonregularFilesWithoutBlocking(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "fifo"
		if alias {
			name = "symlink-to-fifo"
		}
		t.Run(name, func(t *testing.T) {
			for _, operation := range []string{"dry-run-delete", "delete", "content-edit", "recovery"} {
				t.Run(operation, func(t *testing.T) {
					s := newAtomicBatchTestServer(t, mutationTestWatcher{})
					fifo := filepath.Join(t.TempDir(), "source.pipe")
					require.NoError(t, unix.Mkfifo(fifo, 0o600))
					path := fifo
					if alias {
						path = filepath.Join(filepath.Dir(fifo), "link")
						require.NoError(t, os.Symlink(fifo, path))
					}
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					done := make(chan struct{})
					var receipt batchTransactionReceipt
					var result *mcplib.CallToolResult
					var callErr error
					var recoveryStatus string
					go func() {
						defer close(done)
						switch operation {
						case "dry-run-delete":
							req := mcplib.CallToolRequest{}
							req.Params.Arguments = map[string]any{"dry_run": true, "edits": []any{map[string]any{"op": "delete_file", "path": path}}}
							result, callErr = s.handleAtomicBatchEdit(ctx, req)
						case "recovery":
							recoveryStatus, callErr = s.rollbackBatchReceipt(batchTransactionReceipt{Files: []batchTransactionFile{{Path: path, RelativePath: "source.pipe"}}})
						default:
							edit := batchEditItem{Op: "delete_file", Path: path}
							if operation == "content-edit" {
								edit = atomicFileEdit(path, "before", "after")
							}
							receipt, callErr = s.runBatchTransaction(ctx, []batchEditItem{edit}, "refuse-fifo")
						}
					}()
					awaitBatchFIFORefusal(t, fifo, done)
					if operation == "recovery" {
						require.Error(t, callErr)
						assert.Equal(t, "recovery_conflict", recoveryStatus)
					} else {
						require.NoError(t, callErr)
						if operation == "dry-run-delete" {
							assert.Contains(t, viewResultText(t, result), "not a regular file")
						} else {
							assert.Equal(t, "aborted", receipt.Status)
							assert.Equal(t, "unchanged", receipt.DiskStatus)
							assert.Contains(t, receipt.Error, "not a regular file")
						}
					}
					lockCtx, lockCancel := context.WithTimeout(context.Background(), time.Second)
					defer lockCancel()
					release, err := acquireMutationPath(lockCtx, path)
					require.NoError(t, err, "refused batch must release its file lock")
					release()
					info, err := os.Lstat(fifo)
					require.NoError(t, err)
					assert.NotZero(t, info.Mode()&os.ModeNamedPipe)
				})
			}
		})
	}
}

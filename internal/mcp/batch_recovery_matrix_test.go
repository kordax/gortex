package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type batchRecoveryImage struct {
	exists  bool
	content string
	mode    os.FileMode
}

func putBatchRecoveryImage(t *testing.T, path string, image batchRecoveryImage) {
	t.Helper()
	if !image.exists {
		err := os.Remove(path)
		require.True(t, err == nil || errors.Is(err, os.ErrNotExist), "remove %s: %v", path, err)
		return
	}
	require.NoError(t, os.WriteFile(path, []byte(image.content), image.mode))
	require.NoError(t, os.Chmod(path, image.mode))
}

func assertBatchRecoveryImage(t *testing.T, path string, want batchRecoveryImage) {
	t.Helper()
	info, err := os.Lstat(path)
	if !want.exists {
		assert.ErrorIs(t, err, os.ErrNotExist, path)
		return
	}
	require.NoError(t, err, path)
	assert.True(t, info.Mode().IsRegular(), path)
	assert.Equal(t, want.content, readAtomicBatchFixture(t, path), path)
	if os.PathSeparator != '\\' {
		assert.Equal(t, want.mode.Perm(), info.Mode().Perm(), path)
	}
}

// Enumerate every combination of before, after and unrelated disk state for
// an edit, deletion and move. The oracle is independent of the journal's hash
// classifier: any unrelated state freezes all files; a complete after-image
// commits; every other known combination restores the complete before-image.
func TestAtomicBatchRecoveryStateMatrix(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	before := []batchRecoveryImage{
		{true, "edit-before\n", 0o600},
		{true, "delete-before\n", 0o640},
		{true, "move-before\n", 0o640},
		{},
	}
	after := []batchRecoveryImage{
		{true, "edit-after\n", 0o600},
		{},
		{},
		{true, "move-before\n", 0o640},
	}
	for combination := range 81 {
		states := make([]int, len(before))
		number := combination
		var name strings.Builder
		allBefore, allAfter, conflict := true, true, false
		for i := range states {
			states[i] = number % 3
			number /= 3
			name.WriteByte("BAX"[states[i]])
			allBefore = allBefore && states[i] == 0
			allAfter = allAfter && states[i] == 1
			conflict = conflict || states[i] == 2
		}
		t.Run(name.String(), func(t *testing.T) {
			dir := t.TempDir()
			paths := []string{filepath.Join(dir, "edit.txt"), filepath.Join(dir, "delete.txt"), filepath.Join(dir, "source.txt"), filepath.Join(dir, "destination.txt")}
			buffers := make(map[string]*batchFileBuffer, len(paths))
			for i, path := range paths {
				putBatchRecoveryImage(t, path, before[i])
				mode := before[i].mode
				if !before[i].exists {
					mode = after[i].mode
				}
				buffers[path] = &batchFileBuffer{
					absPath: path, relPath: filepath.Base(path), mode: mode,
					original: []byte(before[i].content), content: []byte(after[i].content),
					existsBefore: before[i].exists, existsAfter: after[i].exists, existenceSet: true,
				}
			}
			id := "matrix-" + name.String()
			s := &Server{}
			prepared := batchTransactionReceipt{
				Version: batchTransactionVersion, TransactionID: id, Fingerprint: id,
				StartedAt: time.Now().UTC(),
				Results: []batchEditResult{
					{Op: "edit_file", FilePath: "edit.txt", Status: "validated"},
					{Op: "delete_file", FilePath: "delete.txt", Status: "validated"},
					{Op: "move_file", FilePath: "source.txt", DestinationPath: "destination.txt", Status: "validated"},
				},
			}
			prepared.Summary = batchSummary(prepared.Results)
			require.NoError(t, s.prepareBatchJournal(&prepared, buffers, paths))
			observed := make([]batchRecoveryImage, len(paths))
			for i, path := range paths {
				switch states[i] {
				case 0:
					observed[i] = before[i]
				case 1:
					observed[i] = after[i]
				case 2:
					observed[i] = batchRecoveryImage{true, "external-" + filepath.Base(path), 0o600}
				}
				putBatchRecoveryImage(t, path, observed[i])
			}
			var scheduled atomic.Int64
			restarted := &Server{watcher: mutationTestWatcher{scheduled: &scheduled}, session: newSessionState()}
			receipt, err := restarted.batchTransactionStatus(context.Background(), id)
			require.NoError(t, err)
			assert.True(t, receipt.Recovered)
			want := before
			switch {
			case conflict:
				assert.Equal(t, "recovery_conflict", receipt.Status)
				assert.Equal(t, "conflict", receipt.DiskStatus)
				want = observed
			case allAfter:
				assert.Equal(t, "committed", receipt.Status)
				assert.Equal(t, "committed", receipt.DiskStatus)
				assert.Equal(t, "fresh", receipt.GraphStatus)
				assert.Equal(t, 3, receipt.Summary["applied"])
				want = after
			case allBefore:
				assert.Equal(t, "aborted", receipt.Status)
				assert.Equal(t, "unchanged", receipt.DiskStatus)
			default:
				assert.Equal(t, "aborted", receipt.Status)
				assert.Equal(t, "rolled_back", receipt.DiskStatus)
			}
			if !allAfter {
				assert.Equal(t, "not_started", receipt.GraphStatus)
				assert.Zero(t, scheduled.Load(), "failed recovery must not publish a partial graph")
			} else {
				assert.Equal(t, int64(len(paths)), scheduled.Load())
			}
			for _, server := range []*Server{restarted, {watcher: mutationTestWatcher{scheduled: &scheduled}, session: newSessionState()}} {
				replayed, statusErr := server.batchTransactionStatus(context.Background(), id)
				require.NoError(t, statusErr)
				assert.Equal(t, receipt, replayed, "terminal status must survive another restart")
				for i, path := range paths {
					assertBatchRecoveryImage(t, path, want[i])
				}
			}
			if allAfter {
				assert.Equal(t, int64(len(paths)), scheduled.Load(), "status replay must not re-admit completed publication")
			}
		})
	}
}

// A child exits without Go defers, so recovery sees a real interrupted process,
// rather than a fixture whose normal cleanup accidentally completed the write.
func TestAtomicBatchProcessCrashRecovery(t *testing.T) {
	const childPhaseEnv = "GORTEX_TEST_BATCH_CRASH_PHASE"
	const childRootEnv = "GORTEX_TEST_BATCH_CRASH_ROOT"
	if phase := os.Getenv(childPhaseEnv); phase != "" {
		dir := os.Getenv(childRootEnv)
		require.NotEmpty(t, dir)
		require.NoError(t, os.Setenv(batchTransactionDirEnv, filepath.Join(dir, "transactions")))
		s := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}
		writes := 0
		s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
			if phase == "before-first-write" {
				os.Exit(86)
			}
			if err := durableAtomicWriteFile(path, content, mode); err != nil {
				return err
			}
			writes++
			if (phase == "after-first-write" && writes == 1) || (phase == "after-all-writes" && writes == 3) {
				os.Exit(86)
			}
			return nil
		}
		if phase == "after-commit-manifest" {
			s.batchDurabilityOverride = &batchDurabilityOps{writeFile: func(path string, content []byte, mode os.FileMode) error {
				if err := durableAtomicWriteFile(path, content, mode); err != nil {
					return err
				}
				if filepath.Base(path) == "manifest.json" {
					var receipt batchTransactionReceipt
					if err := json.Unmarshal(content, &receipt); err != nil {
						return err
					}
					if receipt.Status == "committed" {
						os.Exit(86)
					}
				}
				return nil
			}}
		}
		edits := make([]batchEditItem, 3)
		for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
			edits[i] = atomicFileEdit(filepath.Join(dir, name), "before", "after")
		}
		_, err := s.runBatchTransaction(context.Background(), edits, "process-crash-"+phase)
		require.NoError(t, err)
		t.Fatal("crash boundary was never reached")
	}
	self, err := os.Executable()
	require.NoError(t, err)
	for _, phase := range []string{"before-first-write", "after-first-write", "after-all-writes", "after-commit-manifest"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(batchTransactionDirEnv, filepath.Join(dir, "transactions"))
			paths := make([]string, 3)
			for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
				paths[i] = writeAtomicBatchFixture(t, dir, name, "before\n")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestAtomicBatchProcessCrashRecovery$", "-test.count=1")
			cmd.Env = append(os.Environ(), childPhaseEnv+"="+phase, childRootEnv+"="+dir)
			output, childErr := cmd.CombinedOutput()
			require.NoError(t, ctx.Err(), "child failed to terminate: %s", output)
			var exitErr *exec.ExitError
			require.ErrorAs(t, childErr, &exitErr, "%s", output)
			require.Equal(t, 86, exitErr.ExitCode(), "%s", output)
			var scheduled atomic.Int64
			s := &Server{watcher: mutationTestWatcher{scheduled: &scheduled}, session: newSessionState()}
			receipt, statusErr := s.batchTransactionStatus(context.Background(), "process-crash-"+phase)
			require.NoError(t, statusErr)
			want := "before\n"
			if phase == "after-all-writes" || phase == "after-commit-manifest" {
				want = "after\n"
				assert.Equal(t, "committed", receipt.Status)
				assert.Equal(t, "fresh", receipt.GraphStatus)
				assert.Equal(t, int64(len(paths)), scheduled.Load())
			} else {
				assert.Equal(t, "aborted", receipt.Status)
				assert.Zero(t, scheduled.Load())
			}
			for _, path := range paths {
				assert.Equal(t, want, readAtomicBatchFixture(t, path))
			}
			replayed, statusErr := s.batchTransactionStatus(context.Background(), receipt.TransactionID)
			require.NoError(t, statusErr)
			assert.Equal(t, receipt, replayed)
		})
	}
}

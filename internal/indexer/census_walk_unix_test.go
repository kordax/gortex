//go:build unix

package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// A FIFO proves whether language sniffing read an excluded entry. The child
// process bounds a regression without leaving a blocked goroutine in the test
// runner. The timeout is a deadlock guard, not a performance target.
func TestCensusExclusionBeforeSniff(t *testing.T) {
	const helper = "GORTEX_TEST_CENSUS_EXCLUDED_FIFO"
	if mode := os.Getenv(helper); mode != "" {
		root := t.TempDir()
		path := filepath.Join(root, "main.go")
		require.NoError(t, os.WriteFile(path, []byte("package fixture\nfunc Main() {}\n"), 0o600))
		info, err := os.Stat(path)
		require.NoError(t, err)
		if strings.HasSuffix(mode, "_escape") {
			outside := filepath.Join(t.TempDir(), "outside-pipe")
			require.NoError(t, syscall.Mkfifo(outside, 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "unrecognized")))
		} else {
			require.NoError(t, syscall.Mkfifo(filepath.Join(root, "ignored.blob"), 0o600))
		}
		idx := New(graph.New(), newTestRegistry(), config.IndexConfig{Exclude: []string{"**/*.blob"}}, zap.NewNop())
		t.Cleanup(idx.Close)
		idx.SetFileMtimes(map[string]int64{"main.go": info.ModTime().UnixNano()})
		if strings.HasPrefix(mode, "census") {
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			require.Empty(t, changed)
			require.Empty(t, deleted)
			require.Equal(t, 1, detected)
		} else {
			require.False(t, idx.HasChangesSinceMtimes(root))
		}
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, mode := range []string{"census", "short_circuit", "census_escape", "short_circuit_escape"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCensusExclusionBeforeSniff$", "-test.timeout=10s")
			cmd.Env = append(os.Environ(), helper+"="+mode)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "excluded FIFO must never be opened by %s: %s", mode, output)
			require.NoError(t, ctx.Err(), "excluded FIFO caused blocking language detection")
		})
	}
}

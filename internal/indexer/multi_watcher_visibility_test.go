package indexer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMultiWatcherReportsRepoThatNeverStarted is the health half of the macOS
// watcher outage. A per-repo watcher that fails to start was logged once and
// then forgotten: it was absent from `started`, had no DegradedReason of its
// own, and MultiWatcher.Start still returned nil — so the daemon announced
// every configured repo as watched and the graph aged behind a green status.
func TestMultiWatcherReportsRepoThatNeverStarted(t *testing.T) {
	mw, _, repoADir, _ := setupMultiWatcherTest(t)

	// Repo A's root disappears before the watchers come up — a checkout
	// removed under a running daemon.
	require.NoError(t, os.RemoveAll(repoADir))
	require.NoError(t, mw.Start())
	t.Cleanup(func() { _ = mw.Stop() })

	live, configured := mw.WatchedRepos()
	require.Equal(t, 2, configured)
	require.Equal(t, 1, live, "only repo-b can be watched once repo-a's root is gone")

	reason := mw.DegradedReason()
	require.NotEmpty(t, reason, "an unwatched repository must not read as healthy")
	require.Contains(t, reason, "repo-a", "the reason must name the repository that is not being watched")

	// A read inside repo-b must not carry repo-a's outage: the per-file
	// freshness rider asks only about the repository that owns the file.
	require.Contains(t, mw.DegradedReasonFor("repo-a"), "repo-a")
	require.Empty(t, mw.DegradedReasonFor("repo-b"), "a healthy repository must not inherit another repository's watcher failure")
}

// TestMultiWatcherHealthyWhenAllReposStart guards against the inverse: a fully
// started fleet must report no degradation and a live count equal to the
// configured one.
func TestMultiWatcherHealthyWhenAllReposStart(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)

	require.NoError(t, mw.Start())
	t.Cleanup(func() { _ = mw.Stop() })

	live, configured := mw.WatchedRepos()
	require.Equal(t, configured, live)
	require.Equal(t, 2, live)
	require.Empty(t, mw.DegradedReason())
}

package persistence

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSavingsObservationContextWriterAdmission(t *testing.T) {
	s, err := OpenSidecar(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	require.NoError(t, err)
	defer s.Close()
	s.writeMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.AddSavingsObservationContext(ctx, SavingsEvent{Returned: 10, Saved: 20})
	}()
	cancel()
	select {
	case err := <-done:
		s.writeMu.Unlock()
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		s.writeMu.Unlock()
		err := <-done
		t.Fatalf("canceled observation waited for writer gate; result after release: %v", err)
	}
	rows, _, _, err := s.SavingsTotals()
	require.NoError(t, err)
	require.Empty(t, rows)
	ev := SavingsEvent{Repo: "repo", Language: "go", Returned: 10, Saved: 20}
	require.NoError(t, s.AddSavingsObservationContext(context.Background(), ev))
	rows, _, _, err = s.SavingsTotals()
	require.NoError(t, err)
	for _, bucket := range []string{"", "repo:repo", "lang:go"} {
		require.Equal(t, SavingsTotalsRow{Saved: 20, Returned: 10, Calls: 1}, rows[bucket])
	}
	events, err := s.SavingsEventsSince(time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestSavingsObservationRestoresBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	s, err := OpenSidecar(path)
	require.NoError(t, err)
	defer s.Close()
	s.db.SetMaxOpenConns(1)
	var original int
	require.NoError(t, s.db.QueryRow("PRAGMA busy_timeout").Scan(&original))
	require.Greater(t, original, 200)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, s.AddSavingsObservationContext(ctx, SavingsEvent{Saved: 1}))
	var restored int
	require.NoError(t, s.db.QueryRow("PRAGMA busy_timeout").Scan(&restored))
	require.Equal(t, original, restored, "temporary observation budget must not alter shared connection settings")
	other, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer other.Close()
	conn, err := other.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	require.NoError(t, err)
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer blockedCancel()
	require.Error(t, s.AddSavingsObservationContext(blockedCtx, SavingsEvent{Saved: 2}))
	require.NoError(t, s.db.QueryRow("PRAGMA busy_timeout").Scan(&restored))
	require.Equal(t, original, restored, "failed observation must restore connection settings too")
	rows, _, _, err := s.SavingsTotals()
	require.NoError(t, err)
	require.Equal(t, SavingsTotalsRow{Saved: 1, Calls: 1}, rows[""])
}

func TestSavingsObservationContextExpired(t *testing.T) {
	s, err := OpenSidecar(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	require.NoError(t, err)
	defer s.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err = s.AddSavingsObservationContext(ctx, SavingsEvent{Saved: 1})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

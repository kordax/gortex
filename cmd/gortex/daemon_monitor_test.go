package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func TestMonitorStreamReconnectAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	calls := 0
	err := runMonitorStream(ctx, &output, time.Millisecond, false, true, func() monitorSnapshot {
		calls++
		if calls == 1 {
			return monitorSnapshot{Error: "connection refused"}
		}
		cancel()
		return monitorSnapshot{Status: &daemon.StatusResponse{PID: 123, Ready: true}}
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	decoder := json.NewDecoder(&output)
	var failed, recovered monitorSnapshot
	require.NoError(t, decoder.Decode(&failed))
	require.NoError(t, decoder.Decode(&recovered))
	assert.Nil(t, failed.Status)
	assert.Contains(t, failed.Error, "connection refused")
	require.NotNil(t, recovered.Status)
	assert.True(t, recovered.Status.Ready)
	assert.Empty(t, recovered.Error)
}

func TestMonitorStreamOnceUnavailable(t *testing.T) {
	var output bytes.Buffer
	err := runMonitorStream(context.Background(), &output, time.Second, true, false, func() monitorSnapshot {
		return monitorSnapshot{Error: "not reachable", Startup: &daemon.RuntimeState{StartupPhase: daemon.StartupMigrating, MigrationVersion: 22, MigrationName: "foreign key indexes"}}
	})
	require.ErrorContains(t, err, "not reachable")
	assert.Contains(t, output.String(), "migration 22")
	assert.Contains(t, output.String(), "status unavailable")
	assert.NotContains(t, output.String(), "0 indexed files")
}

func TestMonitorCountsUnknownAreNotEmptyRepositories(t *testing.T) {
	status := daemon.StatusResponse{CountsUnknown: true, TrackedRepos: []daemon.TrackedRepoStatus{{Prefix: "fixture", Path: "/fixture"}}}
	text := renderMonitorSnapshot(monitorSnapshot{Status: &status})
	assert.Contains(t, text, "counts unavailable")
	assert.NotContains(t, text, "0 indexed files")
	model := newStatusTUI(time.Second)
	updated, _ := model.Update(tuiStatusMsg(status))
	model = updated.(statusTUI)
	assert.Equal(t, "counts unavailable", model.repos.Items()[0].(repoItem).state)
	model.details = true
	assert.Contains(t, model.renderSelectedRepo(), "counts unavailable")
	assert.NotContains(t, model.renderSelectedRepo(), "0 indexed files")
}

func TestMonitorPauseAndSingleInflightFetch(t *testing.T) {
	model := newStatusTUI(time.Second)
	assert.True(t, model.fetching)
	updated, _ := model.Update(tuiTickMsg(time.Now()))
	model = updated.(statusTUI)
	assert.True(t, model.fetching, "a tick must leave the existing request in flight")
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	model = updated.(statusTUI)
	assert.Nil(t, cmd)
	assert.True(t, model.paused)
	updated, _ = model.Update(tuiStatusMsg(daemon.StatusResponse{PID: 123}))
	model = updated.(statusTUI)
	assert.False(t, model.hasStatus, "an in-flight response cannot change the paused display")
	assert.False(t, model.fetching)
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	model = updated.(statusTUI)
	assert.NotNil(t, cmd)
	assert.False(t, model.paused)
	assert.True(t, model.fetching)
	_, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd(), "exit only terminates the monitor")
	assert.ErrorIs(t, model.fetchContext.Err(), context.Canceled, "exit cancels the monitor's in-flight poll")
}

func TestMonitorReconnectPreservesSelectionAndMarksStale(t *testing.T) {
	model := newStatusTUI(time.Second)
	status := daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{
		{Prefix: "first", Path: "/first"},
		{Prefix: "selected", Path: "/selected"},
	}}
	status.TrackedRepos[0].Memory.TotalBytes = 2
	status.TrackedRepos[1].Memory.TotalBytes = 1
	updated, _ := model.Update(tuiStatusMsg(status))
	model = updated.(statusTUI)
	model.repos.Select(1)
	updated, _ = model.Update(tuiFetchErrMsg{err: errors.New("disconnected")})
	model = updated.(statusTUI)
	assert.True(t, model.hasStatus)
	assert.Contains(t, model.renderMonitorProgress(), "status is stale")
	status.TrackedRepos[1].Memory.TotalBytes = 3
	updated, _ = model.Update(tuiStatusMsg(status))
	model = updated.(statusTUI)
	assert.Nil(t, model.err)
	assert.Equal(t, "/selected", model.repos.SelectedItem().(repoItem).path)
}

func TestMonitorFilteredRowsRefresh(t *testing.T) {
	model := newStatusTUI(time.Second)
	status := daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{{Prefix: "fixture", Path: "/fixture", Files: 1}}}
	updated, _ := model.Update(tuiStatusMsg(status))
	model = updated.(statusTUI)
	model.repos.SetFilterText("fixture")
	status.TrackedRepos[0].Files = 8
	updated, command := model.Update(tuiStatusMsg(status))
	model = updated.(statusTUI)
	require.NotNil(t, command, "the list needs its asynchronous filter refresh command")
	updated, _ = model.Update(command())
	model = updated.(statusTUI)
	require.Len(t, model.repos.VisibleItems(), 1)
	assert.Equal(t, 8, model.repos.VisibleItems()[0].(repoItem).files)
	assert.Equal(t, "/fixture", model.repos.SelectedItem().(repoItem).path)
}

func TestMonitorDetailsToggleAndNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newStatusTUI(time.Second)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 45})
	model = updated.(statusTUI)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(statusTUI)
	assert.True(t, model.details)
	assert.NotContains(t, model.View(), "\x1b[")
	assert.Contains(t, model.View(), "No repository selected")
}

func TestMonitorCommandValidation(t *testing.T) {
	for _, args := range [][]string{{"--interval=1ms"}, {"--json", "--plain"}} {
		cmd := newDaemonMonitorCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "interval") || strings.Contains(err.Error(), "either"))
	}
}

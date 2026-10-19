package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/observability"
)

func TestMonitorCPUTracksProcessIdentity(t *testing.T) {
	previous := observability.ProcessSnapshot{Available: true, PID: 5, StartTicks: 10, SampleUnix: 100, CPUSeconds: 4}
	current := previous
	current.SampleUnix = 102
	current.CPUSeconds = 7
	rate := monitorCPUPercent(&previous, &current)
	require.NotNil(t, rate)
	assert.Equal(t, float64(150), *rate)
	assert.Nil(t, monitorCPUPercent(nil, &current))
	assert.Nil(t, monitorCPUPercent(&previous, &previous))
	current.StartTicks++
	assert.Nil(t, monitorCPUPercent(&previous, &current), "PID reuse must reset CPU deltas")
	current.StartTicks = previous.StartTicks
	current.CPUSeconds = 1
	assert.Nil(t, monitorCPUPercent(&previous, &current), "counter reset must be unknown")
}

func TestMonitorProcessResourcesAndPartialChildren(t *testing.T) {
	status := daemon.StatusResponse{Process: &observability.ProcessSnapshot{Available: true, RSSBytes: 2048, PSSAvailable: true, PSSBytes: 1024, Children: 1, ChildrenRSSBytes: 512, IOAvailable: true, ReadBytes: 128, WriteBytes: 64}}
	text := monitorResourceSummary(status)
	assert.Contains(t, text, "process RSS 2.0 KiB")
	assert.Contains(t, text, "PSS 1.0 KiB")
	assert.Contains(t, text, "I/O read 128 B write 64 B")
	assert.Contains(t, text, "shared pages counted per process")
	assert.Contains(t, text, "child sample incomplete")
	assert.Contains(t, monitorResourceSummary(daemon.StatusResponse{}), "resource sample unavailable")
}

func TestMonitorCanceledFetchDoesNotDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fetchMonitorStatus(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

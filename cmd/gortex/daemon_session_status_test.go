package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

func TestDaemonStatusShowsSessionConnectionsAndRetention(t *testing.T) {
	status := daemon.StatusResponse{
		Sessions: 3, SessionCountsKnown: true,
		SocketSessions: 1, RetainedSessions: 1, TransportSessions: 1,
		MCPSessions: []daemon.MCPSessionStatus{
			{ID: "attached", ClientName: "Codex", ConnectionState: "socket"},
			{ID: "reconnect", ClientName: "Codex", ConnectionState: "retained"},
			{ID: "http", ClientName: "HTTP", ConnectionState: "transport"},
		},
	}
	var header, sessions bytes.Buffer
	renderDaemonHeader(&header, status)
	renderDaemonSessions(&sessions, status)
	assert.Contains(t, header.String(), "3 total: 1 socket, 1 retained, 1 transport")
	for _, text := range []string{"state", "age", "socket", "retained", "transport", "reconnect"} {
		assert.Contains(t, sessions.String(), text)
	}
	assert.NotContains(t, sessions.String(), "connected", "session age must not be described as continuous connection time")
}

func TestDaemonStatusDoesNotInventConnectionCountsForOldDaemon(t *testing.T) {
	var status daemon.StatusResponse
	require.NoError(t, json.Unmarshal([]byte(`{"sessions":2,"mcp_sessions":[{"id":"old","client_name":"Legacy","connected_secs":12}]}`), &status))
	var header, sessions bytes.Buffer
	renderDaemonHeader(&header, status)
	renderDaemonSessions(&sessions, status)
	assert.NotContains(t, header.String(), "0 socket")
	assert.NotContains(t, header.String(), "0 retained")
	assert.Contains(t, sessions.String(), "unknown")
}

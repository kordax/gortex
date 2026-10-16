package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sessionStatusPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, client
}

func TestSessionStatusRetainsReconnectStateWithoutSocket(t *testing.T) {
	registry := NewSessionRegistry()
	control, _ := sessionStatusPipe(t)
	first, _ := sessionStatusPipe(t)
	second, _ := sessionStatusPipe(t)
	registry.Register(control, Handshake{Mode: ModeControl})
	h := Handshake{Mode: ModeMCP, PID: 4242, CWD: "/fixture", LogicalSessionID: "status-logical"}
	sess := registry.Register(first, h)
	sess.SetClientInfo("status-client", "1")
	sess.SessionState = new(int)
	httpSession := registry.RegisterDetached("status-http", Handshake{Mode: ModeMCP})
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}`)
	reply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	calls := 0
	dispatch := func() ([]byte, error) { calls++; return reply, nil }
	_, cached, err := sess.dispatchMCPOnce(request, dispatch)
	require.NoError(t, err)
	require.False(t, cached)

	check := func(total, sockets, retained, transport int) {
		t.Helper()
		status := registry.statusSnapshot(true)
		assert.Equal(t, total, status.total)
		assert.Equal(t, sockets, status.socket)
		assert.Equal(t, retained, status.retained)
		assert.Equal(t, transport, status.transport)
		assert.Equal(t, total, registry.Count(), "legacy count still includes reconnect records")
		states := map[string]int{}
		for _, row := range status.rows {
			states[row.ConnectionState]++
		}
		assert.Equal(t, sockets, states["socket"])
		assert.Equal(t, retained, states["retained"])
		assert.Equal(t, transport, states["transport"])
	}
	check(3, 2, 0, 1)
	require.Nil(t, registry.Remove(first))
	check(3, 1, 1, 1)
	require.False(t, registry.IsAttached(sess.ID))
	rebound := registry.Register(second, h)
	require.Same(t, sess, rebound)
	require.NotNil(t, rebound.SessionState)
	check(3, 2, 0, 1)
	// A late cleanup from the old socket must not detach the replacement.
	require.Nil(t, registry.Remove(first))
	require.True(t, registry.IsAttached(sess.ID))
	got, cached, err := rebound.dispatchMCPOnce(request, dispatch)
	require.NoError(t, err)
	assert.True(t, cached)
	assert.Equal(t, reply, got)
	assert.Equal(t, 1, calls, "rebind must preserve completed-request replay")

	registry.Remove(second)
	check(3, 1, 1, 1)
	dead := registry.SweepDead(func(pid int) bool { return pid != h.PID })
	require.Len(t, dead, 1)
	assert.Same(t, sess, dead[0])
	check(2, 1, 0, 1)
	assert.Same(t, httpSession, registry.RemoveByID(httpSession.ID))
	registry.Remove(control)
	check(0, 0, 0, 0)
}

func TestSessionStatusAndProbeExposeSameBreakdown(t *testing.T) {
	server := New("unused-test-socket", "test", nil)
	server.Controller = &fakeController{}
	server.started = time.Now()
	conn, _ := sessionStatusPipe(t)
	server.sessions.Register(conn, Handshake{Mode: ModeControl})
	logical, _ := sessionStatusPipe(t)
	server.sessions.Register(logical, Handshake{Mode: ModeMCP, PID: 4242, LogicalSessionID: "retained"})
	server.sessions.Remove(logical)
	server.sessions.RegisterDetached("http", Handshake{Mode: ModeMCP})
	for _, kind := range []string{ControlStatus, ControlProbe} {
		t.Run(kind, func(t *testing.T) {
			response := server.handleControl(t.Context(), nil, ControlRequest{Kind: kind})
			require.True(t, response.OK, "%s", response.ErrorMsg)
			var status StatusResponse
			require.NoError(t, json.Unmarshal(response.Result, &status))
			assert.True(t, status.SessionCountsKnown)
			assert.Equal(t, 3, status.Sessions)
			assert.Equal(t, 1, status.SocketSessions)
			assert.Equal(t, 1, status.RetainedSessions)
			assert.Equal(t, 1, status.TransportSessions)
			if kind == ControlStatus {
				require.Len(t, status.MCPSessions, 3)
				states := make([]string, 0, 3)
				for _, row := range status.MCPSessions {
					states = append(states, row.ConnectionState)
				}
				assert.ElementsMatch(t, []string{"socket", "retained", "transport"}, states)
			}
			var legacy struct {
				Sessions int `json:"sessions"`
			}
			require.NoError(t, json.Unmarshal(response.Result, &legacy))
			assert.Equal(t, 3, legacy.Sessions)
		})
	}
	var old StatusResponse
	require.NoError(t, json.Unmarshal([]byte(`{"sessions":3}`), &old))
	assert.False(t, old.SessionCountsKnown, "missing new fields must not imply known zero sockets")
}

func TestSessionStatusSnapshotConcurrentDetach(t *testing.T) {
	registry := NewSessionRegistry()
	conn, _ := sessionStatusPipe(t)
	h := Handshake{Mode: ModeMCP, PID: 4242, LogicalSessionID: "concurrent"}
	registry.RegisterDetached("active-http", Handshake{Mode: ModeMCP})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 300 {
			sess := registry.Register(conn, h)
			sess.SetClientInfo("client", "1")
			registry.Remove(conn)
		}
	}()
	for range 300 {
		snapshot := registry.statusSnapshot(true)
		require.Equal(t, snapshot.total, snapshot.socket+snapshot.retained+snapshot.transport)
		require.Len(t, snapshot.rows, snapshot.total)
		states := map[string]int{}
		for _, row := range snapshot.rows {
			states[row.ConnectionState]++
		}
		require.Equal(t, snapshot.socket, states["socket"])
		require.Equal(t, snapshot.retained, states["retained"])
		require.Equal(t, 1, states["transport"])
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent registry writer did not finish")
	}
}

type sessionStatusBlockingDispatcher struct {
	started, cancelled, release chan struct{}
}

func (d *sessionStatusBlockingDispatcher) Dispatch(ctx context.Context, _ *Session, _ []byte) ([]byte, error) {
	close(d.started)
	select {
	case <-ctx.Done():
		close(d.cancelled)
		return nil, ctx.Err()
	case <-d.release:
		return nil, errors.New("test cleanup released dispatcher")
	}
}

func TestSessionStatusTransportTeardownBeforeLogicalSweep(t *testing.T) {
	server := New("unused-test-socket", "test", nil)
	dispatcher := &sessionStatusBlockingDispatcher{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	server.MCPDispatcher = dispatcher
	serverSide, clientSide := sessionStatusPipe(t)
	require.NoError(t, clientSide.SetDeadline(time.Now().Add(5*time.Second)))
	server.conns[serverSide] = struct{}{}
	done := make(chan struct{})
	go func() { defer close(done); server.handle(serverSide) }()
	t.Cleanup(func() {
		close(dispatcher.release)
		_ = clientSide.Close()
		_ = serverSide.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("connection handler did not exit during cleanup")
		}
	})
	handshake := Handshake{Version: ProtocolVersion, Mode: ModeMCP, PID: 4242, LogicalSessionID: "teardown-retained"}
	require.NoError(t, WriteJSONLine(clientSide, handshake))
	reader := bufio.NewReader(clientSide)
	ackBytes, err := reader.ReadBytes('\n')
	require.NoError(t, err)
	var ack HandshakeAck
	require.NoError(t, json.Unmarshal(ackBytes, &ack))
	require.True(t, ack.OK, "%s", ack.ErrorMsg)
	require.NoError(t, WriteJSONLine(clientSide, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "block"}}))
	select {
	case <-dispatcher.started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach dispatcher")
	}
	require.NoError(t, clientSide.Close())
	select {
	case <-dispatcher.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel the in-flight request")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection handler waited for logical-session sweep")
	}
	snapshot := server.sessions.statusSnapshot(true)
	assert.Equal(t, 0, snapshot.socket)
	assert.Equal(t, 1, snapshot.retained)
	assert.Equal(t, 1, snapshot.total)
	server.connsMu.Lock()
	connections := len(server.conns)
	server.connsMu.Unlock()
	assert.Zero(t, connections, "the retained record must not own a tracked socket")
}

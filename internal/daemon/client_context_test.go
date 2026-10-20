package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func contextTestSocket(t *testing.T) (net.Listener, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("real Unix socket fixture; Windows transport needs platform-specific verification")
	}
	dir, err := os.MkdirTemp("", "gx-dial-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	require.NoError(t, listener.(*net.UnixListener).SetDeadline(time.Now().Add(3*time.Second)))
	return listener, path
}

func TestDialToContextRejectsAlreadyExpiredContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	client, err := DialToContext(ctx, filepath.Join(t.TempDir(), "missing"), Handshake{Mode: ModeControl})
	assert.Nil(t, client)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDialToContextCancellationDuringHandshake(t *testing.T) {
	listener, path := contextTestSocket(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		client, err := DialToContext(ctx, path, Handshake{Mode: ModeControl})
		if client != nil {
			_ = client.Close()
		}
		result <- err
	}()
	conn, err := listener.Accept()
	require.NoError(t, err)
	defer func() {
		_ = conn.Close()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("dial goroutine did not exit")
		}
	}()
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
	var h Handshake
	require.NoError(t, json.NewDecoder(conn).Decode(&h))
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("dial ignored cancellation during handshake")
	}
}

func TestDialToContextDeadlineDuringHandshake(t *testing.T) {
	listener, path := contextTestSocket(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		client, err := DialToContext(ctx, path, Handshake{Mode: ModeControl})
		if client != nil {
			_ = client.Close()
		}
		result <- err
	}()
	conn, err := listener.Accept()
	require.NoError(t, err)
	defer conn.Close()
	// The peer accepts and reads the request but deliberately withholds its
	// ack. The caller's deadline must win over the legacy 10-second budget.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	var h Handshake
	require.NoError(t, json.NewDecoder(conn).Decode(&h))
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("handshake overrode the caller's deadline")
	}
}

func TestDialContextHandsOffConnectionOwnership(t *testing.T) {
	listener, path := contextTestSocket(t)
	t.Setenv("GORTEX_DAEMON_SOCKET", path)
	peerDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peerDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		decoder := json.NewDecoder(conn)
		var h Handshake
		if err := decoder.Decode(&h); err != nil {
			peerDone <- err
			return
		}
		if err := WriteJSONLine(conn, HandshakeAck{OK: true}); err != nil {
			peerDone <- err
			return
		}
		for range 10 {
			var request ControlRequest
			if err := decoder.Decode(&request); err != nil {
				peerDone <- err
				return
			}
			if err := WriteJSONLine(conn, ControlResponse{OK: true}); err != nil {
				peerDone <- err
				return
			}
		}
		peerDone <- nil
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, err := DialContext(ctx, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	defer client.Close()
	cancel()
	// The dial context must no longer own the connection or leave an armed
	// cancellation callback capable of racing the caller's later traffic.
	for range 10 {
		response, err := client.ControlWithTimeout(ControlDashboard, DashboardParams{Action: "status"}, time.Second)
		require.NoError(t, err)
		assert.True(t, response.OK)
	}
	select {
	case err := <-peerDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("peer did not exit")
	}
}

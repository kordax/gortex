package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/testenv"
)

func TestMonitorCancellationClosesInflightControlConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket control transport")
	}
	socket := filepath.Join(testenv.ShortTempDir(t), "monitor.sock")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer listener.Close()
	requestRead := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		if err := daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true}); err != nil {
			serverDone <- err
			return
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		close(requestRead)
		_, err = io.Copy(io.Discard, reader)
		serverDone <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan error, 1)
	go func() {
		_, err := fetchMonitorStatus(ctx)
		clientDone <- err
	}()
	select {
	case <-requestRead:
	case err := <-serverDone:
		t.Fatalf("fake server failed before control request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("control request did not reach private test socket")
	}
	cancel()
	select {
	case err := <-clientDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled status request retained its connection")
	}
	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("private server did not observe client disconnect")
	}
}

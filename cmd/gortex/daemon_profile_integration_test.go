package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

// TestDaemonProfilerIntegration reuses the private daemon/Git fixture and never
// addresses the working daemon. Fast controller and capture tests run normally;
// this process-level check requires an explicitly supplied build.
func TestDaemonProfilerIntegration(t *testing.T) {
	binary := os.Getenv("GORTEX_PROFILE_TEST_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_PROFILE_TEST_BINARY to test an isolated daemon")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the shared process fixture uses Unix interrupt handling")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	f := newIssue767Fixture(t, binary)
	for i := 0; i < len(f.env); i++ {
		if strings.HasPrefix(f.env[i], "GORTEX_DAEMON_PPROF_ADDR=") {
			f.env = append(f.env[:i], f.env[i+1:]...)
			break
		}
	}
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	control := func(action string, args ...string) daemon.ProfilerStatus {
		t.Helper()
		params := append([]string{"daemon", "profile", action, "--json"}, args...)
		output := f.command(10*time.Second, f.primary, params...)
		var status daemon.ProfilerStatus
		require.NoError(t, json.Unmarshal(output, &status), "%s", output)
		return status
	}
	assert.False(t, control("status").Running, "default startup must not open diagnostics")
	status := control("start", "--address", "127.0.0.1:0")
	require.True(t, status.Running)
	for _, kind := range []string{"heap", "cpu"} {
		path := filepath.Join(f.root, kind+".pprof")
		f.command(10*time.Second, f.primary, "daemon", "profile", "--address", status.Address, "--kind", kind, "--duration", "1s", "--out", path)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}), "invalid %s profile", kind)
	}
	assert.False(t, control("stop").Running)
	listener, err := net.Listen("tcp", status.Address)
	require.NoError(t, err, "stopped daemon profiler kept its port")
	require.NoError(t, listener.Close())
	// The same daemon still answers MCP queries after capture and shutdown.
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	assert.True(t, control("start", "--address", status.Address).Running)
	assert.False(t, control("stop").Running)
}

package main

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/diagnostics"
)

func TestDaemonProfileWritesPrivateSnapshotAndPreservesExistingFiles(t *testing.T) {
	server := httptest.NewServer(diagnostics.ProfileHandler())
	defer server.Close()
	path := filepath.Join(t.TempDir(), "heap.pprof")
	run := func() error {
		cmd := newDaemonProfileCommand()
		cmd.SetArgs([]string{"--kind", "heap", "--address", strings.TrimPrefix(server.URL, "http://"), "--out", path})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}
	require.NoError(t, run())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o077, "profiles may contain private process details")
	require.Error(t, run(), "an existing profile must never be silently overwritten")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, after)
}

func TestDaemonProfileRejectsExternalAddressesAndExcessiveDuration(t *testing.T) {
	for _, args := range [][]string{
		{"--address", "192.0.2.1:9475"},
		{"--address", "attacker.example:9475"},
		{"--kind", "cpu", "--duration", "61s"},
		{"--kind", "trace", "--duration", "31s"},
		{"--kind", "cpu", "--duration", "1ms"},
		{"--kind", "invalid"},
	} {
		cmd := newDaemonProfileCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		require.Error(t, cmd.Execute(), args)
	}
}

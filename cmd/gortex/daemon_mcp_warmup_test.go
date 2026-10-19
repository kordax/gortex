package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)

func warmupDispatchReply(t *testing.T, d *mcpDispatcher, sess *daemon.Session, frame string) map[string]any {
	t.Helper()
	reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
	require.NoError(t, err)
	require.NotEmpty(t, reply)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	return parsed
}

func TestDispatcher_WarmupUnknownCWDRecoversWithoutReconnect(t *testing.T) {
	d, mi := trackedPathMCPSetup(t, t.TempDir())
	cwd := t.TempDir()
	sess := &daemon.Session{ID: "warmup-discovery", CWD: cwd}
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	d.srv.PublishReadiness("snapshot_loaded", false, nil)

	initialized := warmupDispatchReply(t, d, sess, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	require.NotContains(t, initialized, "error")
	result, ok := initialized["result"].(map[string]any)
	require.True(t, ok)
	instructions, ok := result["instructions"].(string)
	require.True(t, ok)
	assert.Contains(t, instructions, "warming up")
	assert.Contains(t, instructions, "snapshot_loaded")
	assert.NotContains(t, instructions, "gortex track")
	assert.NotContains(t, instructions, "INACTIVE")

	listed := warmupDispatchReply(t, d, sess, `{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`)
	require.NotContains(t, listed, "error")
	result, ok = listed["result"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, result["tools"])

	reply, err := d.Dispatch(context.Background(), sess, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	require.NoError(t, err)
	assert.Empty(t, reply)

	for _, name := range []string{"read", "capabilities", "workspace_admin"} {
		t.Run(name, func(t *testing.T) {
			frame, marshalErr := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": "pending-" + name, "method": "tools/call",
				"params": map[string]any{"name": name, "arguments": map[string]any{"operation": "track"}},
			})
			require.NoError(t, marshalErr)
			parsed := warmupDispatchReply(t, d, sess, string(frame))
			assert.Equal(t, "pending-"+name, parsed["id"])
			errObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok)
			data, ok := errObj["data"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "daemon_warming_up", data["error_code"])
			assert.Equal(t, true, data["retryable"])
			assert.Equal(t, "snapshot_loaded", data["phase"])
			assert.Equal(t, cwd, data["path"])
			assert.NotContains(t, data, "suggestion")
			encoded, marshalErr := json.Marshal(parsed)
			require.NoError(t, marshalErr)
			assert.NotContains(t, string(encoded), "gortex track")
		})
	}
	_, wronglyLogged := d.loggedUntracked.Load(sess.ID)
	assert.False(t, wronglyLogged, "incomplete discovery must not log a confirmed tracking miss")

	// Simulate the existing warmup loader publishing this repository. It is
	// the same live MCP session, with no reconnect or repeated initialize.
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: cwd})
	require.NoError(t, err)
	d.srv.PublishReadiness("ready", true, nil)
	parsed := warmupDispatchReply(t, d, sess, `{"jsonrpc":"2.0","id":"recovered","method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
	assert.Equal(t, "recovered", parsed["id"])
	require.NotContains(t, parsed, "error", "discovered cwd must be usable without reconnecting")
	assert.Contains(t, parsed, "result")
}

func TestDispatcher_WarmupUnknownCWDStillRejectedAfterReady(t *testing.T) {
	d, _ := trackedPathMCPSetup(t, t.TempDir())
	sess := &daemon.Session{ID: "warmup-stranger", CWD: t.TempDir()}
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	d.srv.PublishReadiness("parallel_parse", false, nil)
	const frame = `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"read","arguments":{}}}`
	pending := warmupDispatchReply(t, d, sess, frame)
	pendingError, ok := pending["error"].(map[string]any)
	require.True(t, ok)
	pendingData, ok := pendingError["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "daemon_warming_up", pendingData["error_code"])

	d.srv.PublishReadiness("ready", true, nil)
	ready := warmupDispatchReply(t, d, sess, frame)
	assert.EqualValues(t, 42, ready["id"])
	readyError, ok := ready["error"].(map[string]any)
	require.True(t, ok)
	data, ok := readyError["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "repo_not_tracked", data["error_code"])
	assert.Contains(t, data["suggestion"], "gortex track")
}

func TestDispatcher_WarmupReadyTransitionDoesNotConfirmStaleMiss(t *testing.T) {
	d, _ := trackedPathMCPSetup(t, t.TempDir())
	sess := &daemon.Session{ID: "warmup-ready-transition", CWD: t.TempDir()}
	d.srv.PublishReadiness("resolve", false, nil)
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) {
		// A registry miss was already observed when the remaining warmup
		// work publishes readiness. It cannot retroactively confirm that miss.
		d.srv.PublishReadiness("ready", true, nil)
		return false, nil
	}
	parsed := warmupDispatchReply(t, d, sess, `{"jsonrpc":"2.0","id":"transition","method":"tools/call","params":{"name":"read","arguments":{}}}`)
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "daemon_warming_up", data["error_code"])
	assert.Equal(t, "resolve", data["phase"])
}

func TestDispatcher_WarmupPreservesCheckoutErrorsAndKnownAdmission(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)
	d.srv.PublishReadiness("resolve", false, nil)
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, context.DeadlineExceeded }
	sess := &daemon.Session{ID: "warmup-checkout-error", CWD: t.TempDir()}
	const frame = `{"jsonrpc":"2.0","id":"checkout","method":"tools/call","params":{"name":"capabilities","arguments":{}}}`
	parsed := warmupDispatchReply(t, d, sess, frame)
	assert.Equal(t, "checkout", parsed["id"])
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "view_building", data["error_code"], "discovery errors must retain their original classification")

	sess.CWD = tracked
	initialized := warmupDispatchReply(t, d, sess, `{"jsonrpc":"2.0","id":"known-init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	require.NotContains(t, initialized, "error")
	parsed = warmupDispatchReply(t, d, sess, frame)
	require.NotContains(t, parsed, "error", "already loaded tracked cwd must keep its normal admission")
	assert.Contains(t, parsed, "result")
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

const admissionReadFrame = `{"jsonrpc":"2.0","id":"read-admission","method":"tools/call","params":{"name":"read","arguments":{"operation":"file","target":{"file":"fixture.go"}}}}`
const admissionInitializeFrame = `{"jsonrpc":"2.0","id":"initialize-admission","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`

// Install an observable handler behind a normal facade tool. Checking its
// execution catches accidental admission even when a later layer would return
// an innocuous protocol or empty-graph response.
func admissionReadCounter(d *mcpDispatcher) *atomic.Int64 {
	calls := new(atomic.Int64)
	d.srv.MCPServer().AddTool(mcplib.NewTool("read"), func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls.Add(1)
		return mcplib.NewToolResultText("admitted"), nil
	})
	return calls
}

func admissionErrorData(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "expected an admission error: %v", parsed)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "expected machine-readable admission data: %v", parsed)
	assert.NotContains(t, parsed, "result")
	return data
}

func TestDispatcher_AdmissionLifecycleDoesNotExecuteUnadmittedTool(t *testing.T) {
	d, mi := trackedPathMCPSetup(t, t.TempDir())
	calls := admissionReadCounter(d)
	sess := &daemon.Session{ID: "admission-state-matrix", CWD: t.TempDir()}
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	d.srv.PublishReadiness("snapshot_loaded", false, nil)
	require.NotContains(t, warmupDispatchReply(t, d, sess, admissionInitializeFrame), "error")

	for _, phase := range []struct {
		name  string
		ready bool
	}{{"snapshot_loaded", false}, {"parallel_parse", false}, {"resolve", false}, {"ready", true}} {
		for _, discovery := range []struct {
			name string
			err  error
			code string
		}{{"missing", nil, ""}, {"busy", indexer.ErrCheckoutMutationBusy, "view_building"}, {"deadline", context.DeadlineExceeded, "view_building"}, {"inaccessible", errors.New("fixture catalog inaccessible"), "checkout_inaccessible"}} {
			t.Run(phase.name+"/"+discovery.name, func(t *testing.T) {
				d.srv.PublishReadiness(phase.name, phase.ready, nil)
				d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, discovery.err }
				want := discovery.code
				if discovery.err == nil {
					want = "daemon_warming_up"
					if phase.ready {
						want = "repo_not_tracked"
					}
				}
				for range 3 {
					parsed := warmupDispatchReply(t, d, sess, admissionReadFrame)
					assert.Equal(t, "read-admission", parsed["id"])
					assert.Equal(t, want, admissionErrorData(t, parsed)["error_code"])
					assert.Zero(t, calls.Load(), "refused calls must not execute the actual tool")
				}
			})
		}
	}

	// All prior refusals belong to this request, not to the session forever.
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: sess.CWD})
	require.NoError(t, err)
	d.srv.PublishReadiness("ready", true, nil)
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	recovered := warmupDispatchReply(t, d, sess, admissionReadFrame)
	require.NotContains(t, recovered, "error")
	assert.EqualValues(t, 1, calls.Load(), "one newly admitted call must execute exactly once")
}

func TestDispatcher_AdmissionCancellationDoesNotPoisonSession(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := "warming"
		if ready {
			name = "ready"
		}
		t.Run(name, func(t *testing.T) {
			tracked := t.TempDir()
			d, mi := trackedPathMCPSetup(t, tracked)
			calls := admissionReadCounter(d)
			sess := &daemon.Session{ID: "admission-cancellation", CWD: t.TempDir()}
			d.srv.PublishReadiness(name, ready, nil)
			d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
			require.NotContains(t, warmupDispatchReply(t, d, sess, admissionInitializeFrame), "error")
			known := &daemon.Session{ID: "admission-independent", CWD: tracked}
			require.NotContains(t, warmupDispatchReply(t, d, known, admissionInitializeFrame), "error")

			entered := make(chan struct{})
			d.checkoutCWDProbe = func(ctx context.Context, _ string) (bool, error) {
				close(entered)
				<-ctx.Done()
				return false, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				reply []byte
				err   error
			}
			done := make(chan result, 1)
			go func() {
				reply, err := d.Dispatch(ctx, sess, []byte(admissionReadFrame))
				done <- result{reply, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("discovery did not start")
			}
			// A blocked discovery for one cwd must not hold up an already
			// admitted session on the same dispatcher.
			independent := make(chan result, 1)
			go func() {
				reply, err := d.Dispatch(context.Background(), known, []byte(admissionReadFrame))
				independent <- result{reply, err}
			}()
			select {
			case outcome := <-independent:
				require.NoError(t, outcome.err)
				var parsed map[string]any
				require.NoError(t, json.Unmarshal(outcome.reply, &parsed))
				require.NotContains(t, parsed, "error")
			case <-time.After(5 * time.Second):
				t.Fatal("independent admitted session blocked behind discovery")
			}
			assert.EqualValues(t, 1, calls.Load())
			cancel()
			select {
			case outcome := <-done:
				require.NoError(t, outcome.err)
				var parsed map[string]any
				require.NoError(t, json.Unmarshal(outcome.reply, &parsed))
				assert.Equal(t, "read-admission", parsed["id"])
				assert.Equal(t, "checkout_inaccessible", admissionErrorData(t, parsed)["error_code"])
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled discovery did not return")
			}
			assert.EqualValues(t, 1, calls.Load(), "cancelled discovery must never reach the tool")

			_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: sess.CWD})
			require.NoError(t, err)
			d.srv.PublishReadiness("ready", true, nil)
			d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
			require.NotContains(t, warmupDispatchReply(t, d, sess, admissionReadFrame), "error")
			assert.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestDispatcher_AdmissionWarmupDoesNotExecuteBootstrapMutation(t *testing.T) {
	d, mi := trackedPathMCPSetup(t, t.TempDir())
	var calls atomic.Int64
	d.srv.MCPServer().AddTool(mcplib.NewTool("workspace_admin"), func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls.Add(1)
		return mcplib.NewToolResultText("mutation reached"), nil
	})
	sess := &daemon.Session{ID: "admission-bootstrap", CWD: t.TempDir()}
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	d.srv.PublishReadiness("snapshot_loaded", false, nil)
	require.NotContains(t, warmupDispatchReply(t, d, sess, admissionInitializeFrame), "error")
	const frame = `{"jsonrpc":"2.0","id":"track-attempt","method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track"}}}`
	for range 3 {
		parsed := warmupDispatchReply(t, d, sess, frame)
		assert.Equal(t, "daemon_warming_up", admissionErrorData(t, parsed)["error_code"])
	}
	assert.Zero(t, calls.Load(), "incomplete discovery must not grant bootstrap mutation access")
	assert.Len(t, mi.AllMetadata(), 1, "refusal must preserve the existing repository registry")

	// Confirm the observation hook is callable on this same session once
	// registry loading has established access, rather than merely hidden.
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: sess.CWD})
	require.NoError(t, err)
	d.srv.PublishReadiness("ready", true, nil)
	require.NotContains(t, warmupDispatchReply(t, d, sess, frame), "error")
	assert.EqualValues(t, 1, calls.Load())
}

func TestDispatcher_AdmissionDeniedSessionCannotUseAnotherSessionReachability(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)
	calls := admissionReadCounter(d)
	known := &daemon.Session{ID: "admission-known", CWD: tracked}
	unknown := &daemon.Session{ID: "admission-unknown", CWD: t.TempDir()}
	d.checkoutCWDProbe = func(context.Context, string) (bool, error) { return false, nil }
	d.srv.PublishReadiness("resolve", false, nil)
	for _, sess := range []*daemon.Session{known, unknown} {
		require.NotContains(t, warmupDispatchReply(t, d, sess, admissionInitializeFrame), "error")
	}

	for _, ready := range []bool{false, true} {
		d.srv.PublishReadiness("session-matrix", ready, nil)
		var wg sync.WaitGroup
		for _, sess := range []*daemon.Session{known, unknown} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 8 {
					reply, err := d.Dispatch(context.Background(), sess, []byte(admissionReadFrame))
					if !assert.NoError(t, err) {
						return
					}
					var parsed map[string]any
					if !assert.NoError(t, json.Unmarshal(reply, &parsed)) {
						return
					}
					if sess == known {
						assert.NotContains(t, parsed, "error")
					} else {
						assert.Contains(t, parsed, "error")
						assert.NotContains(t, parsed, "result")
					}
				}
			}()
		}
		wg.Wait()
	}
	assert.EqualValues(t, 16, calls.Load(), "only the known session may execute the handler")
}

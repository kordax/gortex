package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

const agentBlockedRefreshChild = "GORTEX_BLOCKED_REFRESH_CHILD"

// A subprocess isolates daemon PID files, signal handlers and XDG state. The
// extractor is an ordinary public parser implementation, not a production hook.
func TestAgentBlockedRefreshServerProcess(t *testing.T) {
	root := os.Getenv(agentBlockedRefreshChild)
	if root == "" {
		t.Skip("helper process for TestAgentBatchStatusDuringBlockedRefreshTransport")
	}
	for key, value := range map[string]string{
		"XDG_CONFIG_HOME":  filepath.Join(root, "config"),
		"XDG_DATA_HOME":    filepath.Join(root, "data"),
		"XDG_CACHE_HOME":   filepath.Join(root, "cache"),
		"GORTEX_QUERY_LOG": filepath.Join(root, "queries.jsonl"),
		"GORTEX_TOOLS":     gortexmcp.FacadeSurfaceVersion,
	} {
		t.Setenv(key, value)
	}
	logger := zap.NewNop()
	store, err := store_sqlite.Open(filepath.Join(root, "graph.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	extractor, found := registry.GetByLanguage("go")
	require.True(t, found)
	registry.Register(&agentBlockedRefreshExtractor{Extractor: extractor, root: root, ctx: t.Context()})
	idx := indexer.New(store, registry, config.IndexConfig{}, logger)
	_, err = idx.Index(filepath.Join(root, "repo"))
	require.NoError(t, err)
	engine := query.NewEngine(store)
	engine.SetSearch(idx.Search())
	mcp := gortexmcp.NewServer(engine, store, idx, nil, logger, nil)
	server := daemon.New(filepath.Join(root, "daemon.sock"), "blocked-refresh-test", logger)
	server.MCPDispatcher = newMCPDispatcher(mcp, nil, logger)
	require.NoError(t, server.Listen())
	defer func() { require.NoError(t, server.Shutdown()) }()
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	require.NoError(t, os.WriteFile(filepath.Join(root, "ready"), nil, 0o600))
	var capture sync.Once
	require.Eventually(t, func() bool {
		if _, err := os.Stat(filepath.Join(root, "capture")); err == nil {
			capture.Do(func() {
				profile, err := os.Create(filepath.Join(root, "goroutines.tmp"))
				require.NoError(t, err)
				require.NoError(t, pprof.Lookup("goroutine").WriteTo(profile, 2))
				require.NoError(t, profile.Close())
				require.NoError(t, os.Rename(filepath.Join(root, "goroutines.tmp"), filepath.Join(root, "blocked-goroutines.txt")))
			})
		}
		_, err := os.Stat(filepath.Join(root, "stop"))
		return err == nil
	}, 90*time.Second, 10*time.Millisecond, "parent did not stop isolated daemon")
	require.NoError(t, server.Shutdown())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("isolated daemon did not release its listener")
	}
}

type agentBlockedRefreshExtractor struct {
	parser.Extractor
	root string
	ctx  context.Context
	once sync.Once
}

func (e *agentBlockedRefreshExtractor) Extract(path string, source []byte) (*parser.ExtractionResult, error) {
	if bytes.Contains(source, []byte("AgentBlockedRefreshNew")) {
		body, err := os.ReadFile(filepath.Join(e.root, "repo", "edit.go"))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(body, source) {
			return nil, fmt.Errorf("parser barrier preceded disk commit")
		}
		e.once.Do(func() { _ = os.WriteFile(filepath.Join(e.root, "entered"), nil, 0o600) })
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(80 * time.Second)
		defer deadline.Stop()
		for {
			if _, err := os.Stat(filepath.Join(e.root, "release")); err == nil {
				break
			}
			select {
			case <-ticker.C:
			case <-e.ctx.Done():
				return nil, e.ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("parent did not release parser barrier")
			}
		}
	}
	return e.Extractor.Extract(path, source)
}

// ReadMCPFrame preserves bytes buffered by the daemon handshake reader.
// The writer uses the same real Unix socket after its completed handshake.
type agentDaemonFrameReader struct {
	client  *daemon.Client
	pending []byte
}

func (r *agentDaemonFrameReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		frame, err := r.client.ReadMCPFrame()
		if err != nil {
			return 0, err
		}
		r.pending = frame
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// This exercises canonical single-indexer refresh over the actual daemon Unix
// socket and dispatcher. It is a deadlock invariant, not a latency SLO. A committed receipt must be observable while its owner is
// deliberately unable to finish graph refresh. No user daemon is contacted.
func TestAgentBatchStatusDuringBlockedRefreshTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("isolated Unix socket fixture")
	}
	artifactDir := os.Getenv("GORTEX_ISSUE767_ARTIFACT_DIR")
	if artifactDir != "" {
		require.NoError(t, os.MkdirAll(artifactDir, 0o755))
	}
	root, err := os.MkdirTemp(artifactDir, "gx-blocked-refresh-*")
	require.NoError(t, err)
	if artifactDir == "" {
		t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	}
	t.Logf("isolated blocked-refresh artifacts: %s", root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "repo"), 0o755))
	path := filepath.Join(root, "repo", "edit.go")
	const before = "package fixture\nfunc AgentBlockedRefreshOld() {}\n"
	const after = "package fixture\nfunc AgentBlockedRefreshNew() {}\n"
	require.NoError(t, os.WriteFile(path, []byte(before), 0o644))
	exe, err := os.Executable()
	require.NoError(t, err)
	// t.Context is cancelled before cleanups run. Keep the child alive until
	// cleanup releases its parser and asks it to stop, including on failure.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, exe, "-test.run=^TestAgentBlockedRefreshServerProcess$", "-test.timeout=95s", "-test.v")
	command.Env = append(os.Environ(), agentBlockedRefreshChild+"="+root)
	command.WaitDelay = 5 * time.Second
	log, err := os.Create(filepath.Join(root, "daemon.log"))
	require.NoError(t, err)
	command.Stdout, command.Stderr = log, log
	require.NoError(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0o600)
		_ = os.WriteFile(filepath.Join(root, "stop"), nil, 0o600)
		select {
		case err := <-done:
			if err != nil {
				data, _ := os.ReadFile(log.Name())
				t.Errorf("isolated daemon: %v\n%s", err, data)
			}
		case <-time.After(10 * time.Second):
			cancel()
			t.Error("isolated daemon cleanup timed out")
			if waitErr := <-done; waitErr != nil {
				if ctx.Err() == context.Canceled && command.ProcessState != nil && command.ProcessState.String() == "signal: killed" {
					t.Logf("isolated daemon stopped by forced cleanup cancellation: %v", waitErr)
				} else {
					data, _ := os.ReadFile(log.Name())
					t.Errorf("isolated daemon failed after cleanup cancellation: %v\n%s", waitErr, data)
				}
			}
		}
		_ = log.Close()
		data, _ := os.ReadFile(log.Name())
		if strings.Contains(string(data), "WARNING: DATA RACE") {
			t.Errorf("isolated daemon race: %s", data)
		}
	})
	awaitFile := func(name string) {
		t.Helper()
		require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }, 20*time.Second, 10*time.Millisecond, "missing phase %s", name)
	}
	awaitFile("ready")
	connect := func() *agentWireClient {
		t.Helper()
		connection, err := daemon.DialTo(filepath.Join(root, "daemon.sock"), daemon.Handshake{Mode: daemon.ModeMCP, CWD: filepath.Dir(path)})
		require.NoError(t, err)
		client := newAgentWireClient(&agentDaemonFrameReader{client: connection}, connection.Conn)
		t.Cleanup(func() {
			_ = connection.Close()
			select {
			case <-client.readerDone:
			case <-time.After(5 * time.Second):
				t.Error("Unix MCP frame reader did not stop")
			}
		})
		initCtx, initCancel := context.WithTimeout(ctx, 5*time.Second)
		defer initCancel()
		_, err = client.call(initCtx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "blocked-refresh-test", "version": "1"}})
		require.NoError(t, err)
		require.NoError(t, client.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}))
		return client
	}
	owner, observer := connect(), connect()
	type outcome struct {
		raw json.RawMessage
		err error
	}
	mutation := make(chan outcome, 1)
	const transaction = "agent-blocked-refresh-transaction"
	go func() {
		raw, err := owner.tool(ctx, "edit", map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": path, "old_string": before, "new_string": after}}, "options": map[string]any{"transaction_id": transaction}, "output": map[string]any{"compact": false}})
		mutation <- outcome{raw, err}
	}()
	awaitFile("entered")
	phase, err := json.MarshalIndent(map[string]any{
		"phase": "disk_committed_graph_refresh_parser_held", "transaction_id": transaction,
		"transport": "daemon_unix_socket", "scope": "canonical_single_indexer", "captured_at": time.Now().UTC(),
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "phase.json"), append(phase, '\n'), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capture"), nil, 0o600))
	awaitFile("blocked-goroutines.txt")
	stack, err := os.ReadFile(filepath.Join(root, "blocked-goroutines.txt"))
	require.NoError(t, err)
	require.Contains(t, string(stack), "agentBlockedRefreshExtractor).Extract", "snapshot must prove the owner is held inside graph refresh")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, string(body))
	select {
	case result := <-mutation:
		t.Fatalf("mutation escaped held graph refresh: %v: %s", result.err, result.raw)
	default:
	}
	statusArgs := map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": transaction, "status_only": true}, "output": map[string]any{"compact": false}}
	statusCtx, statusCancel := context.WithTimeout(ctx, 5*time.Second)
	status, err := observer.tool(statusCtx, "edit", statusArgs)
	statusCancel()
	require.NoError(t, err, "status must finish before parser release: %s", status)
	require.NoError(t, os.WriteFile(filepath.Join(root, "status-pending.json"), status, 0o600))
	var value any
	require.NoError(t, json.Unmarshal(status, &value))
	require.True(t, agentJSONField(value, "transaction_id", transaction), "%s", status)
	require.True(t, agentJSONField(value, "disk_status", "committed"), "%s", status)
	require.True(t, agentJSONField(value, "graph_status", "pending"), "%s", status)
	// The observer owns neither the mutation nor its refresh. Disconnect it
	// while the original owner is still held, then poll from a fresh session.
	require.NoError(t, observer.writer.Close())
	observer = connect()
	statusCtx, statusCancel = context.WithTimeout(ctx, 5*time.Second)
	status, err = observer.tool(statusCtx, "edit", statusArgs)
	statusCancel()
	require.NoError(t, err, "reconnected observer must not wait for owner: %s", status)
	require.NoError(t, json.Unmarshal(status, &value))
	require.True(t, agentJSONField(value, "disk_status", "committed"), "%s", status)
	require.True(t, agentJSONField(value, "graph_status", "pending"), "%s", status)
	select {
	case result := <-mutation:
		t.Fatalf("observer disconnect disturbed held owner: %v: %s", result.err, result.raw)
	default:
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "release"), nil, 0o600))
	select {
	case result := <-mutation:
		require.NoError(t, result.err, "%s", result.raw)
	case <-ctx.Done():
		t.Fatal("owner did not finish after parser release")
	}
	status, err = observer.tool(ctx, "edit", statusArgs)
	require.NoError(t, err, "%s", status)
	require.NoError(t, json.Unmarshal(status, &value))
	require.True(t, agentJSONField(value, "graph_status", "fresh"), "%s", status)
	require.NoError(t, os.WriteFile(filepath.Join(root, "status-fresh.json"), status, 0o600))
	body, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, string(body), "status polling must not reapply the mutation")
}

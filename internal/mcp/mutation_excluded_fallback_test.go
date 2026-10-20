package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
)

type fallbackBlockingExtractor struct {
	block   atomic.Bool
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (*fallbackBlockingExtractor) Language() string     { return "go" }
func (*fallbackBlockingExtractor) Extensions() []string { return []string{".go"} }
func (e *fallbackBlockingExtractor) Extract(path string, _ []byte) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == "busy.go" && e.block.Load() {
		e.once.Do(func() { close(e.started) })
		<-e.release
	}
	name := strings.TrimSuffix(filepath.Base(path), ".go")
	return &parser.ExtractionResult{Nodes: []*graph.Node{
		{ID: path, Name: filepath.Base(path), Kind: graph.KindFile, FilePath: path, Language: "go"},
		{ID: path + "::" + name, Name: name, Kind: graph.KindFunction, FilePath: path, Language: "go", StartLine: 2, EndLine: 2},
	}}, nil
}

func excludedFallbackFixture(t *testing.T) (*Server, string, func()) {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", root)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	busyPath := filepath.Join(root, "busy.go")
	require.NoError(t, os.WriteFile(busyPath, []byte("package sample\nfunc Busy() {}\n"), 0o644))
	extractor := &fallbackBlockingExtractor{started: make(chan struct{}), release: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	store := graph.New()
	idx := indexer.New(store, registry, config.IndexConfig{Exclude: []string{"**/*_test.go"}}, zap.NewNop())
	_, err = idx.Index(root)
	require.NoError(t, err)
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Exclude: []string{"**/*_test.go"}}, zap.NewNop())
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	s.watcher = watcher

	extractor.block.Store(true)
	require.NoError(t, os.WriteFile(busyPath, []byte("package sample\nfunc Busy() { println(1) }\n"), 0o644))
	busyDone := make(chan struct{})
	busyErr := make(chan error, 1)
	go func() {
		_, err := idx.IncrementalReindexPaths(root, []string{busyPath})
		busyErr <- err
		close(busyDone)
	}()
	var once sync.Once
	release := func() { once.Do(func() { close(extractor.release) }) }
	t.Cleanup(func() {
		release()
		select {
		case <-busyDone:
			assert.NoError(t, <-busyErr)
		case <-time.After(5 * time.Second):
			t.Error("unrelated reconciliation did not stop after cleanup release")
		}
	})
	select {
	case <-extractor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated reconciliation did not reach blocking extractor")
	}
	return s, root, release
}

type fallbackWriteResult struct {
	result *mcplib.CallToolResult
	err    error
}

func startFallbackWrite(t *testing.T, s *Server, ctx context.Context, path string) (<-chan fallbackWriteResult, <-chan struct{}) {
	t.Helper()
	results := make(chan fallbackWriteResult, 1)
	done := make(chan struct{})
	go func() {
		result, err := s.handleWriteFile(ctx, makeReq("write_file", map[string]any{"path": path, "content": "package sample\nfunc Target() {}\n", "format": "json"}))
		results <- fallbackWriteResult{result: result, err: err}
		close(done)
	}()
	return results, done
}

func TestExcludedPhysicalWriteDoesNotWaitForUnrelatedReconciliation(t *testing.T) {
	s, root, release := excludedFallbackFixture(t)
	path := filepath.Join(root, "new_test.go")
	results, done := startFallbackWrite(t, s, t.Context(), path)
	defer func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("write did not stop after cleanup release")
		}
	}()
	select {
	case outcome := <-results:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.False(t, outcome.result.IsError, toolText(outcome.result))
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(toolText(outcome.result)), &payload))
		assert.Equal(t, "committed", payload["disk_status"])
		assert.NotEmpty(t, payload["mutation_receipt"])
		assert.Equal(t, false, payload["reindexed"])
		assert.Equal(t, true, payload["reindex_skipped"])
		assert.Equal(t, "excluded", payload["reindex_skip_reason"])
		assert.NotEqual(t, "fresh", payload["graph_status"], "physical-only success must not invent indexed freshness")
	case <-time.After(5 * time.Second):
		bytes, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(bytes), "Target")
		assert.True(t, mutationFallbackStackWaiting(), "the delay must be attributable to the synchronous fallback")
		t.Fatal("excluded physical write waited for an unrelated reconciliation executor after disk commit")
	}
	bytes, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(bytes), "Target")
	assert.Empty(t, s.graph.GetFileNodes("new_test.go"), "excluded source must remain unindexed")
}

func mutationFallbackStackWaiting() bool {
	buffer := make([]byte, 1<<20)
	buffer = buffer[:runtime.Stack(buffer, true)]
	for _, stack := range strings.Split(string(buffer), "\n\n") {
		if strings.Contains(stack, ".reindexFile(") && strings.Contains(stack, ".mutationReindexState(") && strings.Contains(stack, ".reconcile(") {
			return true
		}
	}
	return false
}

// Cancellation during the legacy synchronous fallback still waits for its
// coordinator (separately reproduced in the fail-before log). Until a completion
// ticket API separates client waiting from durable work, preserve publication.
func TestNeededNoTicketFallbackCancellationRetainsPublication(t *testing.T) {
	s, root, release := excludedFallbackFixture(t)
	s.watcher = mutationTestWatcher{reject: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := filepath.Join(root, "target.go")
	results, done := startFallbackWrite(t, s, ctx, path)
	defer func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("write did not stop after cleanup release")
		}
	}()
	require.Eventually(t, mutationFallbackStackWaiting, 5*time.Second, time.Millisecond, "request must reach the admitted synchronous fallback before cancellation")
	cancel()
	select {
	case outcome := <-results:
		if outcome.result != nil {
			assert.NotContains(t, toolText(outcome.result), `"reindexed":true`, "blocked publication cannot be reported fresh")
		}
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not complete after executor release")
	}
	assert.NotEmpty(t, s.graph.GetFileNodes("target.go"), "confirmed disk commit must eventually reach the graph even when client stops waiting")
}

type uncertainExclusionWatcher struct {
	mutationTestWatcher
	skip bool
	err  error
}

func (w uncertainExclusionWatcher) CanSkipFileMutation(context.Context, string) (bool, error) {
	return w.skip, w.err
}

func TestUncertainExclusionProofKeepsRequiredFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip bool
		err  error
	}{
		{name: "nil_ticket_only"},
		{name: "lookup_error", skip: true, err: errors.New("test exclusion lookup failed")},
		{name: "lookup_canceled", skip: true, err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, root, release := excludedFallbackFixture(t)
			release()
			s.watcher = uncertainExclusionWatcher{mutationTestWatcher: mutationTestWatcher{reject: true}, skip: tc.skip, err: tc.err}
			path := filepath.Join(root, "target.go")
			results, _ := startFallbackWrite(t, s, t.Context(), path)
			select {
			case outcome := <-results:
				require.NoError(t, outcome.err)
				require.NotNil(t, outcome.result)
				require.False(t, outcome.result.IsError, toolText(outcome.result))
				var payload map[string]any
				require.NoError(t, json.Unmarshal([]byte(toolText(outcome.result)), &payload))
				assert.Equal(t, "committed", payload["disk_status"])
				assert.NotEmpty(t, payload["mutation_receipt"])
				assert.Equal(t, true, payload["reindexed"])
				assert.NotContains(t, payload, "reindex_skipped")
				assert.NotEmpty(t, s.graph.GetFileNodes("target.go"))
			case <-time.After(5 * time.Second):
				t.Fatal("required fallback did not publish after unrelated executor release")
			}
		})
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type agentMixedSample struct {
	Operation string  `json:"operation"`
	Client    int     `json:"client"`
	StartedMS float64 `json:"started_ms"`
	ElapsedMS float64 `json:"elapsed_ms"`
	Error     string  `json:"error,omitempty"`
}

type agentMixedStats struct {
	Samples int     `json:"samples"`
	Errors  int     `json:"errors"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
}

type agentMixedPhase struct {
	WorkloadMS            float64                    `json:"workload_and_profile_ms"`
	CleanupMS             float64                    `json:"cleanup_ms"`
	QueryClients          int                        `json:"query_clients"`
	MutationClients       int                        `json:"mutation_clients"`
	BackpressureResponses int                        `json:"backpressure_responses"`
	Operations            map[string]agentMixedStats `json:"operations"`
	Samples               []agentMixedSample         `json:"samples"`
	SlowProfile           bool                       `json:"slow_profile"`
	ProfileErrors         []string                   `json:"profile_errors,omitempty"`
	RuntimeBefore         json.RawMessage            `json:"runtime_before"`
	RuntimeAfter          json.RawMessage            `json:"runtime_after"`
}

type agentMixedRecorder struct {
	parent      context.Context
	root, addr  string
	start       time.Time
	slow        time.Duration
	captured    atomic.Bool
	cpuMu       sync.Mutex
	mu          sync.Mutex
	samples     []agentMixedSample
	profileErrs []string
}

// Profiles run while a slow request is still pending. The first slow/failing
// request in each phase gets one bounded capture; other requests keep running.
func (r *agentMixedRecorder) capture(operation string) {
	if !r.captured.CompareAndSwap(false, true) {
		return
	}
	triggered := time.Since(r.start)
	for _, profile := range []struct{ endpoint, filename string }{{"goroutine?debug=2", "slow-goroutines.txt"}, {"profile?seconds=2", "slow-cpu.pprof"}} {
		var err error
		if strings.HasPrefix(profile.endpoint, "profile?") {
			err = r.saveCPU(profile.filename)
		} else {
			err = agentSaveProfile(r.parent, r.addr, profile.endpoint, filepath.Join(r.root, profile.filename))
		}
		if err != nil {
			r.mu.Lock()
			r.profileErrs = append(r.profileErrs, fmt.Sprintf("%s: %s: %v", operation, profile.filename, err))
			r.mu.Unlock()
		}
	}
	data, err := json.Marshal(map[string]any{"operation": operation, "threshold_ms": r.slow.Milliseconds(), "started_ms": triggered.Milliseconds()})
	if err == nil {
		err = os.WriteFile(filepath.Join(r.root, "slow-request.json"), append(data, '\n'), 0600)
	}
	if err != nil {
		r.mu.Lock()
		r.profileErrs = append(r.profileErrs, err.Error())
		r.mu.Unlock()
	}
}

func (r *agentMixedRecorder) saveCPU(filename string) error {
	// Go's CPU profiler accepts one recording at a time. Keep the immediate
	// goroutine snapshot independent of an already-running baseline recording.
	r.cpuMu.Lock()
	defer r.cpuMu.Unlock()
	return agentSaveProfile(r.parent, r.addr, "profile?seconds=2", filepath.Join(r.root, filename))
}

func (r *agentMixedRecorder) measure(client int, operation string, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(r.parent, 90*time.Second)
	defer cancel()
	begin := time.Now()
	finished, watcherDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watcherDone)
		timer := time.NewTimer(r.slow)
		defer timer.Stop()
		select {
		case <-finished:
		case <-ctx.Done():
		case <-timer.C:
			r.capture(operation)
		}
	}()
	err := call(ctx)
	sample := agentMixedSample{Operation: operation, Client: client, StartedMS: float64(begin.Sub(r.start).Microseconds()) / 1000, ElapsedMS: float64(time.Since(begin).Microseconds()) / 1000}
	close(finished)
	if err != nil {
		sample.Error = err.Error()
		if len(sample.Error) > 4096 {
			sample.Error = sample.Error[:4096]
		}
		r.capture(operation)
	}
	<-watcherDone
	r.mu.Lock()
	r.samples = append(r.samples, sample)
	r.mu.Unlock()
	return err
}

func agentMixedSource(file, declarations int) string {
	var source strings.Builder
	source.WriteString("package fixture\n")
	if file < 32 {
		fmt.Fprintf(&source, "func Issue767Target%02d() int { return %d }\nfunc Issue767Caller%02d() int { return Issue767Target00() }\n", file, file, file)
	}
	fmt.Fprintf(&source, "type AgentMixedType%05d struct { Value int }\n", file)
	for symbol := range declarations {
		callee := "Issue767Target00()"
		if symbol > 0 {
			callee = fmt.Sprintf("AgentMixed%05d_%03d(value)", file, symbol-1)
		} else if file > 0 {
			callee = fmt.Sprintf("AgentMixed%05d_000(value)", file-1)
		}
		fmt.Fprintf(&source, "func AgentMixed%05d_%03d(value int) int { result := %s; return value + result + Issue767Target00() }\n", file, symbol, callee)
	}
	return source.String()
}

func agentMixedPath(root string, file int) string {
	if file < 32 {
		return filepath.Join(root, fmt.Sprintf("file%02d.go", file))
	}
	return filepath.Join(root, fmt.Sprintf("load%05d.go", file))
}

func agentMixedMutation(value int) string {
	return fmt.Sprintf("package fixture\nfunc AgentMixedMutation() int { return %d }\n", value)
}

func agentMixedInt(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	value := fallback
	if raw := os.Getenv(name); raw != "" {
		var err error
		value, err = strconv.Atoi(raw)
		require.NoError(t, err, "%s", name)
	}
	require.GreaterOrEqual(t, value, minimum, "%s", name)
	require.LessOrEqual(t, value, maximum, "%s", name)
	return value
}

func agentMixedValidate(raw json.RawMessage, requiredText string) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	_, refusal := issue767JSONEvidence(value, "")
	if refusal || !strings.Contains(string(raw), requiredText) {
		return fmt.Errorf("missing expected evidence %q or refused response: %s", requiredText, raw)
	}
	return nil
}

func agentMixedRead(ctx context.Context, client *agentWireClient, path, expected string) error {
	raw, err := client.tool(ctx, "read", map[string]any{"operation": "file", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_tokens": 8000}})
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if err := agentMixedValidate(raw, filepath.Base(path)); err != nil {
		return err
	}
	if !agentJSONField(value, "content", expected) {
		return fmt.Errorf("read did not return exact fixture content for %s: %s", path, raw)
	}
	return nil
}

func agentMixedSummarize(samples []agentMixedSample) map[string]agentMixedStats {
	groups := make(map[string][]float64)
	result := make(map[string]agentMixedStats)
	for _, sample := range samples {
		groups[sample.Operation] = append(groups[sample.Operation], sample.ElapsedMS)
		stats := result[sample.Operation]
		stats.Samples++
		if sample.Error != "" {
			stats.Errors++
		}
		result[sample.Operation] = stats
	}
	for operation, values := range groups {
		sort.Float64s(values)
		stats := result[operation]
		stats.P50MS, stats.P95MS, stats.P99MS = agentPercentile(values, .5), agentPercentile(values, .95), agentPercentile(values, .99)
		stats.MaxMS = values[len(values)-1]
		result[operation] = stats
	}
	return result
}

// TestAgentMixedTransportIntegration shares the isolated fixture and real wire
// client with the race scenario. Its runner defaults to a normal child binary.
func TestAgentMixedTransportIntegration(t *testing.T) {
	binary := os.Getenv("GORTEX_AGENT_TEST_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_AGENT_TEST_BINARY for isolated mixed MCP transport load")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("isolated daemon scenario supports Linux and Darwin")
	}
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < 4*time.Minute {
		t.Fatal("mixed transport scenario needs at least 4m remaining")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	files := agentMixedInt(t, "GORTEX_AGENT_FILES", 256, 32, 10000)
	declarations := agentMixedInt(t, "GORTEX_AGENT_DECLARATIONS", 16, 2, 64)
	rounds := agentMixedInt(t, "GORTEX_AGENT_ROUNDS", 24, 3, 1000)
	clientList := os.Getenv("GORTEX_AGENT_MIXED_CLIENTS")
	if clientList == "" {
		clientList = "1,4,8"
	}
	counts := strings.Split(clientList, ",")
	require.LessOrEqual(t, len(counts), 8)
	var clients []int
	seen := make(map[int]bool)
	for _, raw := range counts {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		require.NoError(t, err, "GORTEX_AGENT_MIXED_CLIENTS")
		require.GreaterOrEqual(t, n, 1)
		require.LessOrEqual(t, n, 32)
		require.False(t, seen[n], "duplicate client count")
		seen[n] = true
		clients = append(clients, n)
	}
	slow := 2 * time.Second
	if raw := os.Getenv("GORTEX_AGENT_SLOW_REQUEST"); raw != "" {
		slow, err = time.ParseDuration(raw)
		require.NoError(t, err)
		require.GreaterOrEqual(t, slow, 100*time.Millisecond)
		require.LessOrEqual(t, slow, 30*time.Second)
	}
	f := newIssue767Fixture(t, binary)
	f.env = append(f.env, "GORTEX_QUERY_LOG="+filepath.Join(f.root, "queries.jsonl"), "GORTEX_DAEMON_SOCKET="+filepath.Join(f.root, "daemon.sock"))
	for file := range files {
		f.write(agentMixedPath(f.primary, file), agentMixedSource(file, declarations))
	}
	mutationPath := filepath.Join(f.primary, "mixed_mutation.go")
	f.write(mutationPath, agentMixedMutation(0))
	// Detect must walk a widely used symbol as well as the changing leaf.
	// Commit an older body, then restore the expected read fixture below.
	firstPath := agentMixedPath(f.primary, 0)
	firstSource := agentMixedSource(0, declarations)
	f.write(firstPath, strings.Replace(firstSource, "func Issue767Target00() int { return 0 }", "func Issue767Target00() int { return -1 }", 1))
	f.git(f.primary, "add", ".")
	f.git(f.primary, "commit", "-m", "mixed transport graph")
	// Keep a known unstaged change throughout all detect requests.
	value := 1
	f.write(firstPath, firstSource)
	f.write(mutationPath, agentMixedMutation(value))
	addr := agentProfileAddress(t)
	for i, entry := range f.env {
		if strings.HasPrefix(entry, "GORTEX_DAEMON_PPROF_ADDR=") {
			f.env[i] = "GORTEX_DAEMON_PPROF_ADDR=" + addr
		}
	}
	t.Cleanup(func() {
		f.stop()
		entries, err := os.ReadDir(f.root)
		require.NoError(t, err)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".log") {
				data, err := os.ReadFile(filepath.Join(f.root, entry.Name()))
				require.NoError(t, err)
				require.NotContains(t, string(data), "WARNING: DATA RACE", "child log %s", entry.Name())
			}
		}
	})
	startup := func() float64 {
		begin := time.Now()
		f.start()
		f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
		client, closeClient := f.agentClient(f.primary)
		defer closeClient()
		f.await("last generated symbol indexed and file readable", 2*time.Minute, func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			name := fmt.Sprintf("AgentMixed%05d_%03d", files-1, declarations-1)
			raw, err := client.tool(ctx, "search", agentSearchArgs(name, f.primary, f.primary))
			if err != nil {
				return false
			}
			var evidence any
			if json.Unmarshal(raw, &evidence) != nil {
				return false
			}
			found, fallback := issue767JSONEvidence(evidence, name)
			return found && !fallback && agentJSONField(evidence, "absolute_file_path", agentMixedPath(f.primary, files-1)) && agentMixedRead(ctx, client, agentMixedPath(f.primary, files-1), agentMixedSource(files-1, declarations)) == nil
		})
		return float64(time.Since(begin).Microseconds()) / 1000
	}
	cold := startup()
	f.stop()
	warm := startup()
	status := agentDaemonStatusReader(t, f)
	f.await("startup proxy sessions released", 2*time.Minute, func() bool { return status().SocketSessions == 1 })
	db := f.openReadOnly()
	var nodes, edges int64
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM nodes").Scan(&nodes))
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM edges").Scan(&edges))
	require.NoError(t, db.Close())
	// Prove the profiling endpoint belongs to a responsive private daemon before load.
	agentCaptureProfile(t, f, addr, "goroutine?debug=2", "before-goroutines.txt")
	var phases []agentMixedPhase
	errors := 0
	for _, count := range clients {
		phase := agentMixedPhase{QueryClients: count, MutationClients: 1, RuntimeBefore: status().Raw}
		pool := make([]*agentWireClient, count)
		closers := make([]func(), count)
		for i := range pool {
			pool[i], closers[i] = f.agentClient(f.primary)
		}
		writer, closeWriter := f.agentClient(f.primary)
		root := filepath.Join(f.root, fmt.Sprintf("clients-%d", count))
		require.NoError(t, os.MkdirAll(root, 0700))
		recorder := &agentMixedRecorder{parent: t.Context(), root: root, addr: addr, start: time.Now(), slow: slow}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for index, client := range pool {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for round := range rounds {
					operation := []string{"search", "read", "change.detect"}[(round+index)%3]
					err := recorder.measure(index, operation, func(ctx context.Context) error {
						switch operation {
						case "search":
							return agentCheckSearch(ctx, client, "Issue767PrimaryMarker", f.primary, f.primary, true)
						case "read":
							file := (round*31 + index*17) % files
							return agentMixedRead(ctx, client, agentMixedPath(f.primary, file), agentMixedSource(file, declarations))
						default:
							raw, err := client.tool(ctx, "change", map[string]any{"operation": "detect", "source": map[string]any{"scope": "unstaged"}, "options": map[string]any{"repo": "issue767", "limit": 8}, "output": map[string]any{"format": "json", "max_tokens": 3000}})
							if err != nil {
								return err
							}
							return agentMixedValidate(raw, "AgentMixedMutation")
						}
					})
					if err != nil {
						break
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for mutation := range max(2, rounds/6) {
				transaction := fmt.Sprintf("mixed-%d-%d-%d", os.Getpid(), count, mutation)
				err := recorder.measure(count, "edit.batch", func(ctx context.Context) error {
					_, err := writer.tool(ctx, "edit", map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": mutationPath, "old_string": agentMixedMutation(value), "new_string": agentMixedMutation(value + 1)}}, "options": map[string]any{"transaction_id": transaction}, "output": map[string]any{"compact": false}})
					return err
				})
				// Resolve the original ID even if the initial request timed out.
				statusErr := recorder.measure(count, "edit.status", func(ctx context.Context) error {
					raw, err := writer.tool(ctx, "edit", map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": transaction, "status_only": true}, "output": map[string]any{"compact": false}})
					if err != nil {
						return err
					}
					var ledger any
					if err := json.Unmarshal(raw, &ledger); err != nil {
						return err
					}
					if !agentJSONField(ledger, "transaction_id", transaction) || !agentJSONField(ledger, "disk_status", "committed") {
						return fmt.Errorf("unexpected batch ledger: %s", raw)
					}
					return nil
				})
				if err != nil || statusErr != nil {
					return
				}
				value++
			}
		}()
		profileDone := make(chan error, 1)
		go func() { profileDone <- recorder.saveCPU("cpu.pprof") }()
		close(start)
		wg.Wait()
		if err := <-profileDone; err != nil {
			recorder.profileErrs = append(recorder.profileErrs, err.Error())
		}
		phase.WorkloadMS = float64(time.Since(recorder.start).Microseconds()) / 1000
		phase.Samples, phase.ProfileErrors, phase.SlowProfile = recorder.samples, recorder.profileErrs, recorder.captured.Load()
		sort.Slice(phase.Samples, func(i, j int) bool { return phase.Samples[i].StartedMS < phase.Samples[j].StartedMS })
		phase.Operations = agentMixedSummarize(phase.Samples)
		for _, stats := range phase.Operations {
			errors += stats.Errors
		}
		cleanupBegin := time.Now()
		for i, client := range append(pool, writer) {
			client.mu.Lock()
			phase.BackpressureResponses += client.busyResponses
			client.mu.Unlock()
			if i < len(closers) {
				closers[i]()
			} else {
				closeWriter()
			}
		}
		writePhase := func() {
			data, err := json.MarshalIndent(phase, "", "  ")
			require.NoError(t, err)
			f.write(filepath.Join(root, "phase.json"), string(data)+"\n")
		}
		writePhase() // Preserve completed requests even if cleanup fails.
		summary, err := json.Marshal(phase.Operations)
		require.NoError(t, err)
		t.Logf("mixed phase query_clients=%d mutation_clients=1 backpressure=%d: %s", count, phase.BackpressureResponses, summary)
		f.await("mixed proxy sessions released", 2*time.Minute, func() bool { return status().SocketSessions == 1 })
		phase.RuntimeAfter = status().Raw
		phase.CleanupMS = float64(time.Since(cleanupBegin).Microseconds()) / 1000
		writePhase()
		phases = append(phases, phase)
	}
	data, err := json.MarshalIndent(map[string]any{"files": files, "declarations_per_file": declarations, "nodes": nodes, "edges": edges, "rounds": rounds, "cold_ms": cold, "warm_ms": warm, "slow_threshold_ms": slow.Milliseconds(), "errors": errors, "phases": phases}, "", "  ")
	require.NoError(t, err)
	f.write(filepath.Join(f.root, "mixed-transport.json"), string(data)+"\n")
	agentCaptureProfile(t, f, addr, "heap", "heap.pprof")
	agentCaptureProfile(t, f, addr, "goroutine?debug=2", "goroutines.txt")
	content, err := os.ReadFile(mutationPath)
	require.NoError(t, err)
	require.Equal(t, agentMixedMutation(value), string(content))
	require.Zero(t, errors, "mixed workload failures; see %s/mixed-transport.json", f.root)
	for _, phase := range phases {
		require.Empty(t, phase.ProfileErrors, "profile capture failures")
	}
}

func TestAgentMixedFixtureTypeChecks(t *testing.T) {
	fileset := token.NewFileSet()
	var files []*ast.File
	for file := range 3 {
		parsed, err := parser.ParseFile(fileset, fmt.Sprintf("file%d.go", file), agentMixedSource(file, 4), parser.AllErrors)
		require.NoError(t, err)
		files = append(files, parsed)
	}
	_, err := new(types.Config).Check("example.invalid/mixed", fileset, files, nil)
	require.NoError(t, err, "generated fixture must have valid cross-file calls")
}

func TestAgentMixedCapturesPendingRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		observed <- request.URL.Path
		_, _ = w.Write([]byte("local profile fixture"))
	}))
	defer server.Close()
	recorder := &agentMixedRecorder{parent: ctx, root: t.TempDir(), addr: strings.TrimPrefix(server.URL, "http://"), start: time.Now(), slow: 10 * time.Millisecond}
	err := recorder.measure(0, "pending-test", func(ctx context.Context) error {
		// The request cannot finish until the watchdog has requested both
		// profiles. No machine-dependent elapsed-time assertion is needed.
		for range 2 {
			select {
			case <-observed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, recorder.captured.Load())
	require.Empty(t, recorder.profileErrs)
	require.Len(t, recorder.samples, 1)
	for _, filename := range []string{"slow-goroutines.txt", "slow-cpu.pprof", "slow-request.json"} {
		data, err := os.ReadFile(filepath.Join(recorder.root, filename))
		require.NoError(t, err)
		require.NotEmpty(t, data)
	}
}

func TestAgentMixedEvidenceRejectsErrors(t *testing.T) {
	require.NoError(t, agentMixedValidate(json.RawMessage(`{"content":[{"type":"text","text":"{\"name\":\"AgentMixedMutation\"}"}]}`), "AgentMixedMutation"))
	require.Error(t, agentMixedValidate(json.RawMessage(`{"error_code":"timeout","message":"AgentMixedMutation"}`), "AgentMixedMutation"))
	require.Error(t, agentMixedValidate(json.RawMessage(`{"freshness":{"exact":false},"name":"AgentMixedMutation"}`), "AgentMixedMutation"))
	require.Error(t, agentMixedValidate(json.RawMessage(`{"results":[]}`), "AgentMixedMutation"))
}

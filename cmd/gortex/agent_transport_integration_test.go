package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// agentWireClient speaks actual newline-delimited MCP JSON-RPC over a proxy's
// stdin/stdout. One reader dispatches by ID, including out-of-order replies and
// late replies to cancelled requests. It never dials the user's daemon itself.
type agentWireClient struct {
	writer        io.WriteCloser
	writeMu       sync.Mutex
	mu            sync.Mutex
	next          int
	pending       map[int]chan agentWireReply
	done          chan struct{}
	readErr       error
	readerDone    chan struct{}
	busyResponses int
}

type agentRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		DeliveryUnknown *bool  `json:"delivery_unknown"`
		Outcome         string `json:"outcome"`
		Retryable       bool   `json:"retryable"`
	} `json:"data"`
}

func (e *agentRPCError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }
func (e *agentRPCError) undeliveredBusy() bool {
	return e.Code == -32002 && e.Data.Outcome == "server_busy" && e.Data.Retryable && e.Data.DeliveryUnknown != nil && !*e.Data.DeliveryUnknown
}

type agentWireReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (reply agentWireReply) response() (json.RawMessage, error) {
	if len(reply.Error) != 0 && string(reply.Error) != "null" {
		var rpcErr agentRPCError
		if err := json.Unmarshal(reply.Error, &rpcErr); err != nil {
			return nil, fmt.Errorf("invalid RPC error: %w", err)
		}
		return nil, &rpcErr
	}
	return reply.Result, nil
}

func newAgentWireClient(reader io.Reader, writer io.WriteCloser) *agentWireClient {
	c := &agentWireClient{writer: writer, pending: make(map[int]chan agentWireReply), done: make(chan struct{}), readerDone: make(chan struct{})}
	go func() {
		defer close(c.readerDone)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		for scanner.Scan() {
			var reply agentWireReply
			if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
				c.fail(fmt.Errorf("invalid MCP frame: %w", err))
				return
			}
			if reply.ID == 0 { // Server notifications carry no request ID.
				continue
			}
			c.mu.Lock()
			ch := c.pending[reply.ID]
			delete(c.pending, reply.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- reply
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		c.fail(err)
	}()
	return c
}

func (c *agentWireClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr == nil {
		c.readErr = err
		close(c.done)
	}
}

func (c *agentWireClient) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if writer, ok := c.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := writer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
	}
	_, err = c.writer.Write(append(data, '\n'))
	return err
}

func (c *agentWireClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	ch := make(chan agentWireReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.response()
	case <-ctx.Done():
		// Sending is bounded by the owning process context/pipe closure. The
		// scenario's messages are small and its peer continuously drains stdin.
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": ctx.Err().Error()}})
		return nil, ctx.Err()
	case <-c.done:
		// EOF may follow a fully framed reply before this goroutine is
		// scheduled. Deliver that buffered reply rather than losing it to EOF.
		select {
		case reply := <-ch:
			return reply.response()
		default:
		}
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		return nil, err
	}
}

func (c *agentWireClient) tool(ctx context.Context, name string, args any) (json.RawMessage, error) {
	var result json.RawMessage
	var err error
	for attempt := 0; ; attempt++ {
		result, err = c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
		var rpcErr *agentRPCError
		if !errors.As(err, &rpcErr) || !rpcErr.undeliveredBusy() || attempt >= 100 {
			break
		}
		c.mu.Lock()
		c.busyResponses++
		c.mu.Unlock()
		timer := time.NewTimer(time.Duration(min(attempt+1, 5)) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		return nil, err
	}
	var envelope struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, err
	}
	if envelope.IsError {
		return result, fmt.Errorf("tool %s refused: %s", name, result)
	}
	return result, nil
}

func (f *issue767Fixture) agentClient(dir string) (*agentWireClient, func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.t.Context())
	if deadline, ok := f.t.Deadline(); ok {
		cancel()
		ctx, cancel = context.WithDeadline(f.t.Context(), deadline.Add(-time.Minute))
	}
	cmd := exec.CommandContext(ctx, f.binary, "mcp", "--proxy", "--tools", gortexmcp.FacadeSurfaceVersion)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, f.env, 5*time.Second
	log, err := os.CreateTemp(f.root, "proxy-*.log")
	require.NoError(f.t, err)
	cmd.Stderr = log
	stdin, err := cmd.StdinPipe()
	require.NoError(f.t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(f.t, err)
	require.NoError(f.t, cmd.Start())
	c := newAgentWireClient(stdout, stdin)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	closeClient := func() {
		once.Do(func() {
			_ = stdin.Close()
			cancel()
			select {
			case err := <-done:
				if err != nil && ctx.Err() == nil {
					f.t.Errorf("proxy exit: %v", err)
				}
			case <-time.After(10 * time.Second):
				f.t.Errorf("proxy %d did not exit", cmd.Process.Pid)
			}
			select {
			case <-c.readerDone:
			case <-time.After(5 * time.Second):
				f.t.Error("MCP frame reader did not exit")
			}
			_ = log.Close()
		})
	}
	f.t.Cleanup(closeClient)
	initCtx, initCancel := context.WithTimeout(f.t.Context(), 20*time.Second)
	defer initCancel()
	result, err := c.call(initCtx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "gortex-agent-test", "version": "1"}})
	require.NoError(f.t, err, "initialize: %s; proxy log %s", result, log.Name())
	require.NoError(f.t, c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}))
	return c, closeClient
}

func agentSearchArgs(name, root, primary string) map[string]any {
	args := map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"limit": 10, "query_class": "symbol", "expand": "off"}, "output": map[string]any{"format": "json"}}
	if root != primary {
		args["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	return args
}

func agentCheckSearch(ctx context.Context, c *agentWireClient, name, root, primary string, want bool) error {
	raw, err := c.tool(ctx, "search", agentSearchArgs(name, root, primary))
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	found, fallback := issue767JSONEvidence(value, name)
	if fallback {
		return fmt.Errorf("fallback/error searching %s: %s", name, raw)
	}
	if found != want {
		return fmt.Errorf("search %s found=%v want=%v: %s", name, found, want, raw)
	}
	if root != primary && !issue767JSONExact(value) {
		return fmt.Errorf("worktree search lacks exact freshness: %s", raw)
	}
	if found && !issue767JSONSource(value, name, root) {
		return fmt.Errorf("wrong source identity: %s", raw)
	}
	return nil
}

type agentLatencyReport struct {
	BackpressureResponses int       `json:"backpressure_responses"`
	Clients               int       `json:"clients"`
	Samples               int       `json:"samples"`
	Errors                int       `json:"errors"`
	ColdMS                float64   `json:"cold_ms"`
	WarmMS                float64   `json:"warm_ms"`
	P50MS                 float64   `json:"p50_ms"`
	P95MS                 float64   `json:"p95_ms"`
	P99MS                 float64   `json:"p99_ms"`
	DurationsMS           []float64 `json:"durations_ms"`
}

func agentPercentile(sorted []float64, percentile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := max(0, min(len(sorted)-1, int(math.Ceil(percentile*float64(len(sorted))))-1))
	return sorted[index]
}

// TestAgentTransportIntegration is deliberately opt-in. The ordinary suite runs
// the deterministic wire-client tests below and the handler cancellation tests.
// The child daemon and all proxy clients inherit the fixture's private XDG dirs.
func TestAgentTransportIntegration(t *testing.T) {
	binary := os.Getenv("GORTEX_AGENT_TEST_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_AGENT_TEST_BINARY to run isolated MCP transport load")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("isolated daemon scenario supports Linux and Darwin")
	}
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < 4*time.Minute {
		t.Fatal("transport scenario needs at least 4m remaining")
	}
	binary, err := filepath.Abs(binary)
	require.NoError(t, err)
	clients, rounds, files := 3, 10, 32
	for _, setting := range []struct {
		name string
		dst  *int
		max  int
	}{{"GORTEX_AGENT_CLIENTS", &clients, 32}, {"GORTEX_AGENT_ROUNDS", &rounds, 1000}, {"GORTEX_AGENT_FILES", &files, 10000}} {
		if value := os.Getenv(setting.name); value != "" {
			n, err := strconv.Atoi(value)
			require.NoError(t, err)
			minimum := 1
			if setting.name == "GORTEX_AGENT_FILES" {
				minimum = 32
			}
			require.GreaterOrEqual(t, n, minimum)
			require.LessOrEqual(t, n, setting.max)
			*setting.dst = n
		}
	}
	f := newIssue767Fixture(t, binary)
	t.Cleanup(func() {
		// Proxy cleanups run first (registered later). Stop the daemon before
		// checking logs: child race-detector failures must fail the parent test.
		f.stop()
		entries, err := os.ReadDir(f.root)
		if err != nil {
			t.Errorf("read child logs: %v", err)
			return
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".log") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(f.root, entry.Name()))
			if err != nil {
				t.Errorf("child log %s: %v", entry.Name(), err)
				continue
			}
			if strings.Contains(string(data), "WARNING: DATA RACE") {
				t.Errorf("race detector reported a child-process race; see %s", filepath.Join(f.root, entry.Name()))
			}
		}
	})
	// Keep query telemetry private even if the parent test sandbox supplies it.
	f.env = append(f.env, "GORTEX_QUERY_LOG="+filepath.Join(f.root, "queries.jsonl"), "GORTEX_DAEMON_SOCKET="+filepath.Join(f.root, "daemon.sock"))
	for i := 32; i < files; i++ {
		f.write(filepath.Join(f.primary, fmt.Sprintf("load%05d.go", i)), fmt.Sprintf("package fixture\nfunc AgentLoad%05d() int { return Issue767Target00() }\n", i))
	}
	f.git(f.primary, "add", ".")
	f.git(f.primary, "commit", "--allow-empty", "-m", "transport load size")
	profileAddr := agentProfileAddress(t)
	for i, entry := range f.env {
		if strings.HasPrefix(entry, "GORTEX_DAEMON_PPROF_ADDR=") {
			f.env[i] = "GORTEX_DAEMON_PPROF_ADDR=" + profileAddr
		}
	}
	var failureProfile sync.Once
	captureFailure := func() {
		failureProfile.Do(func() {
			if err := agentSaveProfile(t.Context(), profileAddr, "goroutine?debug=2", filepath.Join(f.root, "failure-goroutines.txt")); err != nil {
				t.Logf("failure goroutine profile unavailable: %v", err)
			}
			if err := agentSaveProfile(t.Context(), profileAddr, "profile?seconds=1", filepath.Join(f.root, "failure-cpu.pprof")); err != nil {
				t.Logf("failure CPU profile unavailable: %v", err)
			}
		})
	}
	report := agentLatencyReport{Clients: clients}
	cold := time.Now()
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	first, closeFirst := f.agentClient(f.primary)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	require.NoError(t, agentCheckSearch(ctx, first, "Issue767PrimaryMarker", f.primary, f.primary, true))
	cancel()
	report.ColdMS = float64(time.Since(cold).Microseconds()) / 1000
	closeFirst()
	f.stop()
	warm := time.Now()
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	pool := make([]*agentWireClient, clients)
	closers := make([]func(), clients)
	pool[0], closers[0] = f.agentClient(f.primary)
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
	require.NoError(t, agentCheckSearch(ctx, pool[0], "Issue767PrimaryMarker", f.primary, f.primary, true))
	cancel()
	report.WarmMS = float64(time.Since(warm).Microseconds()) / 1000
	for i := 1; i < len(pool); i++ {
		pool[i], closers[i] = f.agentClient(f.primary)
	}
	readStatus := agentDaemonStatusReader(t, f)
	before := readStatus()
	// Read-only calls overlap filesystem mutations and calls from other sessions.
	writer, closeWriter := f.agentClient(f.primary)
	start := make(chan struct{})
	type sample struct {
		ms  float64
		err error
	}
	results := make(chan sample, clients*rounds)
	var wg sync.WaitGroup
	for _, c := range pool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < rounds; n++ {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				begin := time.Now()
				err := agentCheckSearch(ctx, c, "Issue767PrimaryMarker", f.primary, f.primary, true)
				elapsed := float64(time.Since(begin).Microseconds()) / 1000
				cancel()
				if err != nil {
					captureFailure()
				}
				results <- sample{elapsed, err}
			}
		}()
	}
	profileDone := make(chan error, 1)
	go func() {
		profileDone <- agentSaveProfile(t.Context(), profileAddr, "profile?seconds=1", filepath.Join(f.root, "cpu.pprof"))
	}()
	close(start)
	// The mutation uses an already-connected MCP session and a stable transaction ID.
	mutationPath := filepath.Join(f.primary, "marker.go")
	transactionID := "agent-transport-" + strconv.Itoa(os.Getpid())
	ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
	mutation, mutationErr := writer.tool(ctx, "edit", map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": mutationPath, "old_string": issue767MarkerSource("Issue767PrimaryMarker"), "new_string": issue767MarkerSource("Issue767PrimaryMarker", "AgentTransportEditedMarker")}}, "options": map[string]any{"transaction_id": transactionID}, "output": map[string]any{"compact": false}})
	cancel()
	// Always inspect the ledger before any retry, including a timeout response.
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
	status, statusErr := writer.tool(ctx, "edit", map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": transactionID, "status_only": true}, "output": map[string]any{"compact": false}})
	cancel()
	if mutationErr != nil || statusErr != nil {
		captureFailure()
	}
	wg.Wait()
	close(results)
	for result := range results {
		report.DurationsMS = append(report.DurationsMS, result.ms)
		if result.err != nil {
			report.Errors++
			t.Logf("concurrent query: %v", result.err)
		}
	}
	for _, client := range append(pool, writer) {
		client.mu.Lock()
		report.BackpressureResponses += client.busyResponses
		client.mu.Unlock()
	}
	require.NoError(t, mutationErr, "mutation response %s; ledger %s", mutation, status)
	require.NoError(t, statusErr, "ledger %s", status)
	require.NoError(t, <-profileDone, "CPU profile during load")
	var ledger any
	require.NoError(t, json.Unmarshal(status, &ledger))
	require.True(t, agentJSONField(ledger, "transaction_id", transactionID), "wrong ledger transaction: %s", status)
	require.True(t, agentJSONField(ledger, "disk_status", "committed"), "batch was not committed: %s", status)
	content, err := os.ReadFile(mutationPath)
	require.NoError(t, err)
	require.Contains(t, string(content), "AgentTransportEditedMarker")
	closeWriter()
	f.awaitSymbol(f.primary, "AgentTransportEditedMarker")
	// A protocol error and a cancellation must leave subsequent replies usable.
	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
	_, err = pool[0].call(ctx, "agent-test/unknown-method", map[string]any{})
	cancel()
	require.Error(t, err)
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err = pool[0].call(ctx, "tools/list", map[string]any{})
	require.ErrorIs(t, err, context.Canceled)
	// Send cancellation across the real transport too. The request may finish
	// before the notification; deterministic in-flight cancellation is covered
	// by TestAgentWireClientMultiplexCancelAndDisconnect and handler tests.
	require.NoError(t, pool[0].send(map[string]any{"jsonrpc": "2.0", "id": 1000000, "method": "tools/call", "params": map[string]any{"name": "search", "arguments": agentSearchArgs("Issue767PrimaryMarker", f.primary, f.primary)}}))
	require.NoError(t, pool[0].send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 1000000, "reason": "transport cancellation smoke"}}))
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
	require.NoError(t, agentCheckSearch(ctx, pool[0], "AgentTransportEditedMarker", f.primary, f.primary, true))
	cancel()
	// Race a request with proxy termination, then create a fresh real session.
	// The deterministic in-flight disconnect case is tested with net.Pipe below.
	pending := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		_, err := pool[0].call(ctx, "tools/list", map[string]any{})
		pending <- err
	}()
	closers[0]()
	select {
	case err := <-pending:
		if err != nil {
			t.Logf("request released on proxy disconnect: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("disconnected request did not release")
	}
	pool[0], closers[0] = f.agentClient(f.primary)
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
	require.NoError(t, agentCheckSearch(ctx, pool[0], "AgentTransportEditedMarker", f.primary, f.primary, true))
	cancel()
	f.git(f.primary, "worktree", "add", "-b", "agent-transport-linked", f.linked)
	f.write(filepath.Join(f.linked, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker", "AgentTransportLinkedMarker"))
	f.awaitSymbol(f.linked, "AgentTransportLinkedMarker")
	linked, closeLinked := f.agentClient(f.linked)
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
	require.NoError(t, agentCheckSearch(ctx, linked, "AgentTransportLinkedMarker", f.linked, f.primary, true))
	require.NoError(t, agentCheckSearch(ctx, pool[0], "AgentTransportLinkedMarker", f.primary, f.primary, false))
	cancel()
	closeLinked()
	agentContendedEdits(t, f, pool[0])
	for _, closeClient := range closers {
		closeClient()
	}
	f.git(f.primary, "worktree", "remove", "--force", f.linked)
	f.awaitRemoved()
	f.await("proxy sessions released", 2*time.Minute, func() bool { return readStatus().SocketSessions == before.SocketSessions-len(pool) })
	after := readStatus()
	f.write(filepath.Join(f.root, "runtime.json"), fmt.Sprintf("{\"before\":%s,\"after\":%s}\n", before.Raw, after.Raw))
	agentCaptureProfile(t, f, profileAddr, "goroutine?debug=2", "goroutines.txt")
	agentCaptureProfile(t, f, profileAddr, "heap", "heap.pprof")
	sort.Float64s(report.DurationsMS)
	report.Samples = len(report.DurationsMS)
	report.P50MS, report.P95MS, report.P99MS = agentPercentile(report.DurationsMS, .50), agentPercentile(report.DurationsMS, .95), agentPercentile(report.DurationsMS, .99)
	data, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	f.write(filepath.Join(f.root, "transport.json"), string(data)+"\n")
	t.Logf("MCP transport report: %s", data)
	require.Zero(t, report.Errors, "concurrent read failures")
}

func agentContendedEdits(t *testing.T, f *issue767Fixture, first *agentWireClient) {
	t.Helper()
	second, closeSecond := f.agentClient(f.primary)
	defer closeSecond()
	start := make(chan struct{})
	type outcome struct {
		result json.RawMessage
		err    error
	}
	results := make(chan outcome, 2)
	for i, client := range []*agentWireClient{first, second} {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			result, err := client.tool(ctx, "edit", map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": filepath.Join(f.primary, "marker.go"), "old_string": "func AgentTransportEditedMarker()", "new_string": fmt.Sprintf("func AgentContended%d()", i)}}, "options": map[string]any{"transaction_id": fmt.Sprintf("agent-conflict-%d-%d", os.Getpid(), i)}, "output": map[string]any{"compact": false}})
			results <- outcome{result, err}
		}()
	}
	close(start)
	committed, aborted := 0, 0
	for range 2 {
		var value any
		response := <-results
		require.NoError(t, json.Unmarshal(response.result, &value), "contention must return a transaction outcome: %v", response.err)
		if agentJSONField(value, "status", "committed") && agentJSONField(value, "disk_status", "committed") {
			committed++
		}
		if agentJSONField(value, "status", "aborted") && agentJSONField(value, "disk_status", "unchanged") {
			aborted++
		}
	}
	require.Equal(t, 1, committed, "exactly one conflicting edit must commit")
	require.Equal(t, 1, aborted, "the losing optimistic edit must leave disk unchanged")
	content, err := os.ReadFile(filepath.Join(f.primary, "marker.go"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(content), "func AgentContended"))
	require.Contains(t, string(content), "func Issue767PrimaryMarker()")
	for i := range 2 {
		name := fmt.Sprintf("AgentContended%d", i)
		if strings.Contains(string(content), "func "+name+"()") {
			f.awaitSymbol(f.primary, name)
		}
	}
}

type agentStatus struct {
	SessionCountsKnown bool            `json:"session_counts_known"`
	SocketSessions     int             `json:"socket_sessions"`
	RetainedSessions   int             `json:"retained_sessions"`
	TransportSessions  int             `json:"transport_sessions"`
	Sessions           int             `json:"sessions"`
	Raw                json.RawMessage `json:"-"`
}

func agentDaemonStatusReader(t *testing.T, f *issue767Fixture) func() agentStatus {
	t.Helper()
	// One explicit private control connection keeps status polling from
	// creating the very sessions whose cleanup the scenario observes.
	client, err := daemon.DialTo(filepath.Join(f.root, "daemon.sock"), daemon.Handshake{Mode: daemon.ModeControl, CWD: f.primary})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return func() agentStatus {
		t.Helper()
		response, err := client.ControlWithTimeout(daemon.ControlStatus, nil, 10*time.Second)
		require.NoError(t, err)
		require.True(t, response.OK, "%s", response.ErrorMsg)
		var result agentStatus
		require.NoError(t, json.Unmarshal(response.Result, &result))
		require.True(t, result.SessionCountsKnown, "rebuild the test binary with session connection counters")
		result.Raw = append(json.RawMessage(nil), response.Result...)
		return result
	}
}

func agentProfileAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func agentCaptureProfile(t *testing.T, f *issue767Fixture, addr, endpoint, filename string) {
	t.Helper()
	require.NoError(t, agentSaveProfile(t.Context(), addr, endpoint, filepath.Join(f.root, filename)))
}

func agentSaveProfile(parent context.Context, addr, endpoint, filename string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/debug/pprof/"+endpoint, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("profile %s: HTTP %d", endpoint, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(filename, body, 0600)
}

func agentJSONField(value any, key, want string) bool {
	switch value := value.(type) {
	case map[string]any:
		if value[key] == want {
			return true
		}
		for _, child := range value {
			if agentJSONField(child, key, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if agentJSONField(child, key, want) {
				return true
			}
		}
	case string:
		var nested any
		if json.Unmarshal([]byte(value), &nested) == nil {
			return agentJSONField(nested, key, want)
		}
	}
	return false
}

func TestAgentWireClientMultiplexCancelAndDisconnect(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()
	c := newAgentWireClient(clientSide, clientSide)
	serverDone := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(serverSide)
		var first, second struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := decoder.Decode(&first); err != nil {
			serverDone <- err
			return
		}
		if err := decoder.Decode(&second); err != nil {
			serverDone <- err
			return
		}
		encoder := json.NewEncoder(serverSide)
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": second.ID, "result": second.Method}); err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": first.ID, "result": first.Method}); err != nil {
			serverDone <- err
			return
		}
		var cancelled struct {
			ID int `json:"id"`
		}
		if err := decoder.Decode(&cancelled); err != nil {
			serverDone <- err
			return
		}
		var notification struct {
			Method string `json:"method"`
			Params struct {
				RequestID int `json:"requestId"`
			} `json:"params"`
		}
		if err := decoder.Decode(&notification); err != nil {
			serverDone <- err
			return
		}
		if notification.Method != "notifications/cancelled" || notification.Params.RequestID != cancelled.ID {
			serverDone <- errors.New("wrong cancellation frame")
			return
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": cancelled.ID, "result": "late"}); err != nil {
			serverDone <- err
			return
		}
		var final struct {
			ID int `json:"id"`
		}
		if err := decoder.Decode(&final); err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": final.ID, "result": "healthy"}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- serverSide.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, method := range []string{"first", "second"} {
		go func() {
			result, err := c.call(ctx, method, nil)
			if err == nil && string(result) != strconv.Quote(method) {
				err = fmt.Errorf("misrouted reply: %s", result)
			}
			results <- err
		}()
	}
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	short, shortCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer shortCancel()
	_, err := c.call(short, "cancel-me", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	result, err := c.call(ctx, "after-cancel", nil)
	require.NoError(t, err)
	require.JSONEq(t, `"healthy"`, string(result))
	require.NoError(t, <-serverDone)
	select {
	case <-c.readerDone:
	case <-ctx.Done():
		t.Fatal("reader leaked after disconnect")
	}
	_, err = c.call(ctx, "after-close", nil)
	require.ErrorIs(t, err, io.EOF)
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	require.Zero(t, pending)
}

type agentWireWriterFunc func([]byte) (int, error)

func (write agentWireWriterFunc) Write(data []byte) (int, error) { return write(data) }
func (agentWireWriterFunc) Close() error                         { return nil }

func TestAgentWireClientBufferedReplyBeforeEOF(t *testing.T) {
	for range 100 {
		client := &agentWireClient{pending: make(map[int]chan agentWireReply), done: make(chan struct{})}
		client.writer = agentWireWriterFunc(func(data []byte) (int, error) {
			var request struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				return 0, err
			}
			client.pending[request.ID] <- agentWireReply{ID: request.ID, Result: json.RawMessage(`{"ok":true}`)}
			client.fail(io.EOF)
			return len(data), nil
		})
		// Both reply and EOF are ready before call enters its select.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		result, err := client.call(ctx, "final-request", nil)
		cancel()
		require.NoError(t, err)
		require.JSONEq(t, `{"ok":true}`, string(result))
	}
}

func TestAgentWireClientDisconnectDuringRequest(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	client := newAgentWireClient(clientSide, clientSide)
	serverDone := make(chan error, 1)
	go func() {
		var request json.RawMessage
		if err := json.NewDecoder(serverSide).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		serverDone <- serverSide.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.call(ctx, "outstanding-request", nil)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, <-serverDone)
	select {
	case <-client.readerDone:
	case <-ctx.Done():
		t.Fatal("reader leaked on in-flight disconnect")
	}
	client.mu.Lock()
	count := len(client.pending)
	client.mu.Unlock()
	require.Zero(t, count)
}

func TestAgentBusyRetryRequiresExplicitNondelivery(t *testing.T) {
	for _, tc := range []struct {
		payload string
		retry   bool
	}{
		{`{"code":-32002,"data":{"outcome":"server_busy","retryable":true,"delivery_unknown":false}}`, true},
		{`{"code":-32002,"data":{"outcome":"server_busy","retryable":true,"delivery_unknown":true}}`, false},
		{`{"code":-32002,"data":{"outcome":"server_busy","retryable":true}}`, false},
		{`{"code":-32002,"data":{"outcome":"server_busy","delivery_unknown":false}}`, false},
		{`{"code":-32603,"data":{"outcome":"server_busy","retryable":true,"delivery_unknown":false}}`, false},
	} {
		var rpcErr agentRPCError
		require.NoError(t, json.Unmarshal([]byte(tc.payload), &rpcErr))
		require.Equal(t, tc.retry, rpcErr.undeliveredBusy(), tc.payload)
	}
}

func TestAgentPercentile(t *testing.T) {
	require.Zero(t, agentPercentile(nil, .99))
	require.Equal(t, float64(1), agentPercentile([]float64{1}, .99))
	require.Equal(t, float64(2), agentPercentile([]float64{1, 2, 3, 4}, .50))
	require.Equal(t, float64(4), agentPercentile([]float64{1, 2, 3, 4}, .99))
}

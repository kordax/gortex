# Isolated agent transport scenario

Run from the repository root:

```bash
bash scripts/test-agent-transport.sh
```

The script builds a private race-enabled binary and starts a daemon with its own
SQLite database, XDG directories, query log and explicit Unix socket. Each client
is a separate `gortex mcp --proxy --tools facade-v1` process. Requests cross real
stdio framing and the daemon socket. No installed daemon, working repository,
external model endpoint or LLM call is used.

The scenario reuses the fixture from `TestIssue767IdleIOIntegration`. It checks:

- Cold and warm startup through the first successful MCP search.
- Independent sessions issuing parallel searches while another session commits a
  batch, with the exact source path and repository identity checked in replies.
- Batch transaction status lookup with the original transaction ID and proof of
  committed disk status. A timed-out mutation is never retried blindly.
- A protocol error followed by a healthy reply, cancellation notifications over
  the wire, proxy termination and a new session that can query again.
- Automatic linked worktree discovery, exact view freshness and no primary-view
  leakage, followed by worktree removal.
- Two conflicting optimistic batch edits: exactly one commits and the other
  aborts without writing, preserving the stable declaration.
- Proxy process and frame-reader termination, and release of daemon socket
  connections. Logical reconnect records may remain until the dead-peer sweep.

The client retries only explicit dispatcher admission refusals carrying
`server_busy`, `retryable=true` and `delivery_unknown=false`, with bounded backoff.
These refusals are counted separately as `backpressure_responses`; reported
latencies include retry waits. Transport timeouts, uncertain delivery and wrong
search results are never retried by this policy.

In-flight cancellation, disconnects, late replies and out-of-order response routing also have
fast deterministic tests in the normal `go test` suite. The real cancellation
smoke request may finish before its cancellation arrives. Deterministic handler
timeout, rollback and transaction-status cases remain in `internal/mcp`; the
transport scenario does not force a mutation to time out. Its request can also
finish before proxy termination; a deterministic unit closes the wire after
receiving a request and before replying. Fresh-session recovery
does not replace the existing daemon tests for reconnecting a logical session.

Configure the load and artifact directory through environment variables:

```bash
GORTEX_AGENT_CLIENTS=8 \
GORTEX_AGENT_ROUNDS=100 \
GORTEX_AGENT_FILES=512 \
GORTEX_AGENT_ARTIFACT_DIR=/tmp/gortex-agent-run \
bash scripts/test-agent-transport.sh
```

Defaults are 3 clients, 10 queries per client and 32 generated source files.
Bounds are 1 to 32 clients, 1 to 1000 queries per client and 32 to 10000 files.
The fixed fixture also contains a marker file and `go.mod`. Larger inputs may
need a longer `go test -timeout`; the script uses 20 minutes and child cleanup
starts before the test deadline. Run the `Agent transport` GitHub Actions workflow
manually for the same scenario in CI. Ordinary CI runs the fast tests without
launching a daemon.

Artifacts include daemon and proxy logs, `transport.json` with every latency
sample, p50/p95/p99 and error count, `runtime.json` with before/after runtime
counters, a CPU profile captured during the workload, and final heap/goroutine
snapshots. The first failed workload request also triggers bounded goroutine and
CPU captures (`failure-goroutines.txt`, `failure-cpu.pprof`) while the failure is
still being investigated; profile collection never retries the failed request. Set `GORTEX_AGENT_ARTIFACT_DIR` to keep a predictable destination; the
script prints its generated directory otherwise. Profiles are bounded requests
to a loopback-only endpoint belonging to the temporary daemon. If another
process claims the chosen profiling port between reservation and startup, the
scenario fails rather than silently omitting profiles.

```bash
go tool pprof -top /tmp/gortex-agent-run/gx767-*/cpu.pprof
```

Latency measures include MCP framing and dispatch. Startup measurements include
fixture readiness checks and client initialization. They are diagnostic samples,
not performance SLOs. No machine-dependent percentile or goroutine-count limit is
asserted; process/session cleanup and response correctness are hard assertions.
The fixture's accelerated janitor also makes it unsuitable for inferring normal
idle I/O. A short fast run cannot exclude rare production stalls or establish the
cause of a previously observed 59-second request.

Cold embedding loads are covered separately by local HTTP-server tests in
`internal/embedding/cold_load_test.go`: delayed responses, HTTP/checksum errors,
truncated bodies, stalled headers/bodies, cancellation, concurrent downloads and
shared-loader lifecycle. They need no external network or cached model.

Test-target analysis distinguishes graph evidence from execution coverage.
Excluded or unindexed test files yield unknown evidence, rather than proving no
tests exist. Import a runtime coverage profile when execution coverage is needed;
the project's test-file exclusion is intentionally retained.

## Mixed workload with a normal binary

```bash
GORTEX_AGENT_SCENARIO=mixed \
GORTEX_AGENT_MIXED_CLIENTS=1,4,8 \
GORTEX_AGENT_FILES=256 \
GORTEX_AGENT_DECLARATIONS=16 \
GORTEX_AGENT_ROUNDS=24 \
bash scripts/test-agent-transport.sh
```

This mode defaults to a normal build without the race detector. Set
`GORTEX_AGENT_RACE=1` for a separate correctness run. Each phase has the selected
number of query clients plus one mutation client, all independent proxy
processes. Query clients cycle through symbol search, exact file reads and
`change.detect` over known unstaged changes to a shared target and a mutation
leaf. Every generated function calls that shared target, so detection includes
substantial dependency fanout. The writer performs sequential
batch edits and checks each original transaction ID, including after errors.
No semantic miss or uncertain mutation is retried.

The generated Go graph has cross-file calls, local variables and declarations
in each file. Defaults are 256 source files, 16 generated functions per file and
24 requests per query client; the fixture also has marker and mutation files.
Actual persisted node/edge counts are included in `mixed-transport.json`, along
with cold/warm startup, every request, per-operation percentiles, errors,
backpressure and runtime/session counters. Workload/profile time and proxy
cleanup time are recorded separately. Each `clients-N/phase.json` is also
saved before checking session cleanup, preserving completed samples if cleanup
fails. The script allows 20 minutes. In the manual `Agent transport` workflow,
choose `mixed`; its client count selects one phase with an additional writer.

Each phase saves a CPU profile. The first request lasting at least
`GORTEX_AGENT_SLOW_REQUEST` (default `2s`, allowed `100ms` to `30s`) also triggers
a goroutine snapshot and a bounded CPU profile while it is pending. A failed
request triggers the same diagnostic capture if none has started. This is a
profiling trigger, not a pass/fail latency limit. Profile collection can affect
later requests; its own time after a reply is excluded from that reply's latency.
Requests have a 90-second deadline. Profiles stay in the private fixture's
`clients-N` directories. A fast synthetic run does not explain or rule out the
previously observed 59-second calls against a larger production graph.

## Connection cleanup and reconnect records

Status and probe keep the existing `sessions` total and add `socket_sessions`,
`retained_sessions`, `transport_sessions` and `session_counts_known`. The known
flag is absent on older daemons: missing counters mean unavailable evidence,
not zero connections. Session rows expose `connection_state` as `socket`,
`retained` or `transport`. A transport-managed HTTP session is active even though
it has no daemon-owned socket.

Both scenarios use one persistent private control connection and wait until
only the expected socket connection remains. Their runtime snapshots retain
all counters, including reconnect records. They do not wait for logical records
to be purged: that cleanup runs every 30 seconds and has a different purpose.
Fast daemon tests prove that disconnect cancels an in-flight request and exits
the connection handler before logical cleanup, while state and completed-request
replay survive a later rebind. The reconnect policy and sweep interval are
unchanged.

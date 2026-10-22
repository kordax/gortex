# Repeatable local performance measurements

Run from any directory, passing a new artifact directory:

```sh
sh scripts/bench-performance.sh --out /tmp/gortex-baseline
# After a change, on the same machine and toolchain:
sh scripts/bench-performance.sh --out /tmp/gortex-candidate \
  --compare /tmp/gortex-baseline/summary.json
```

The runner uses Go 1.27.2 unless `GOTOOLCHAIN` is explicitly set, four Go CPUs,
200 ms per benchmark and five repetitions. Packages execute sequentially;
`BenchmarkParallelSearch` deliberately exercises concurrent reads inside its
own process. Close other heavy workloads before comparing runs. The runner
records CPU model, architecture, logical CPU count, Go version, Go CPU setting,
commit and tracked working-tree diff hash. Uncommitted new files are not
identified by that diff hash: preserve the checkout alongside the report.

`micro.txt` contains raw Go benchmark results; `summary.json` preserves each
sample, median, minimum/maximum time and the before/after percentage change.
Both allocation bytes and allocation counts matter. A shorter latency is not a
successful optimization if correctness checks fail. Comparisons reject changed
CPU/toolchain/parallelism metadata or fixture versions. Even matching metadata
cannot prove equivalent thermal, OS-cache or background-load conditions. Deltas
are descriptive: this command has no automatic machine-independent timing gate.

## Workloads

- `bench/workloads`: deterministic generated Go trees with 32/128 source files
  and eight functions per file; real SQLite-backed indexing and symbol search.
  Fixture setup, database creation and shutdown are outside the cold-index timer.
  Cold means a new index/store, not an emptied operating-system page cache.
- Warm search includes the actual query engine/reranking and checks exact hits.
  Parallel search reports aggregate throughput as ns/op, not individual-request
  p95 latency. Every fixture uses private testenv storage; no working daemon,
  user repository, embedding download or LLM is involved.
- `internal/observability`: request-accounting, metric exposition and cached
  resource-snapshot overhead. These distinguish instrumentation overhead from
  query work.

Go test's benchmark calibration may run a case several times before reporting
one sample. Do not interpret `--count` as the number of fixture initializations.

## Real multi-agent transport

```sh
sh scripts/bench-performance.sh --suite all --transport-runs 3 \
  --clients 1,4,8 --files 64 --declarations 8 --rounds 24 \
  --out /tmp/gortex-with-transport
```

Use `--suite transport` to omit microbenchmarks. Each transport run reuses
[scripts/test-agent-transport.sh](../../scripts/test-agent-transport.sh) in mixed
mode with a normal, non-race binary. It creates private Git repositories, XDG
storage, SQLite, a daemon and independent stdio proxy processes. Each phase has
1/4/8 query clients plus one writer by default. Source reads, search and impact
queries overlap transactional mutations. Each operation validates its result;
errors fail the run. Explicit undelivered admission refusals have bounded retries
and separate backpressure counts. Unknown mutation delivery is never retried.

The artifact tree includes raw per-request timings, operation p50/p95/p99, errors,
backpressure, cold/warm startup, runtime snapshots and CPU/goroutine profiles.
The top-level summary retains every transport report. Small percentile samples
are diagnostic only; a p99 from 64 calls is effectively an extreme observation,
not a production SLO. Profile capture and generator CPU can influence latency.
Transport reports are not automatically compared by `--compare`.

Do not run these measurements with `-race` and call them release performance.
Run race-enabled correctness checks separately. All daemon/profile addresses
belong to the temporary fixture; this command does not install a binary or
contact the maintainer's daemon.

## Focused profiles and regressions

```sh
GOTOOLCHAIN=go1.27.3 GOMAXPROCS=4 go test ./bench/workloads \
  -run '^$' -bench '^BenchmarkWarmSearch/files=128$' -cpu=4 \
  -benchtime=2s -count=1 -cpuprofile=/tmp/gortex-search.cpu.pprof \
  -memprofile=/tmp/gortex-search.mem.pprof -o /tmp/gortex-workloads.test

go tool pprof -top /tmp/gortex-workloads.test /tmp/gortex-search.cpu.pprof
go tool pprof -alloc_space -top /tmp/gortex-workloads.test /tmp/gortex-search.mem.pprof

GOTOOLCHAIN=go1.27.3 go test -race ./bench/workloads
python3 -m unittest discover -s bench/performance -p 'test_*.py'
```

Profiles include untimed fixture setup and benchmark validation as well as the
measured loop. Separate those stacks before attributing a cost to request work.
The first profile identified per-search construction of an immutable SQL LIKE
escaper. Sharing it preserved literal wildcard handling and reduced warm-search
allocation bytes by about 11% in an exploratory local comparison. Whole-query
latency did not measurably improve in that comparison. Larger remaining costs
were SQLite LIKE work and graph-edge hydration for reranking.

The next experiment added an optional ranking-only adjacency projection. It
keeps every edge and its confidence, decodes metadata only for `similar_to`, and
caches empty adjacency within the request. Full edge APIs keep all metadata;
readers without the projection use their existing view-aware reads. Tests compare
all ranking signals, scores and ordering against full hydration, plus complete
query-engine results. Chunking, structural filtering, generation isolation and
indexed query plans have separate regression checks.

A paired control keeps every other SQLite capability active and changes only
ranking hydration, sharing the same indexed fixture and process:

```sh
GOTOOLCHAIN=go1.27.3 GOMAXPROCS=4 go test ./bench/workloads \
  -run '^$' -bench '^BenchmarkRankingProjection$' -benchmem \
  -benchtime=300ms -count=7 -cpu=4
```

One local seven-sample run measured median full/projected search at about
355/343 microseconds, 55.9/49.3 KB allocated and 1114/996 allocations per query.
That is about 12% fewer allocation bytes beyond the earlier LIKE change; the
observed 3% time reduction is descriptive, not a portable performance guarantee.
The historical 59-second MCP delay has not been reproduced or explained by these
small fixtures.

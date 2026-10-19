# One-file incremental indexing

These workloads use temporary source trees, private SQLite databases and the real parser and resolver. They do not start a daemon, track repositories or call external models.

Run the correctness checks first:

```sh
GOTOOLCHAIN=go1.27.2 go test -race -timeout=10m ./internal/indexer \
  -run 'Test(Incremental(Performance|RestoresType)|DerivedPublicationFrontier|Contract(EnvOwners|OwnerRefresh))' -count=1
```

The tests compare relationship identities with a cold rebuild after declaration, body and documentation edits and unchanged saves. Separate memory and SQLite checks cover inferred implementations, method overrides, inherited entry-point stamps and repeated passes without duplicates. Process execution and environment-consumer relationships must survive publication, while renamed or removed contracts retire their old ownership and preserve sibling consumers. Graph test links are not execution coverage.

Structural publication retires incident derived edges even when a family's semantic fingerprint is unchanged. `DerivedInvalidationPlan.ReplacedFiles` carries the exact restoration frontier alongside the semantic flags. Metadata-only refreshes preserve their edges and leave this frontier empty. An unchanged contract registry entry still needs its actual graph ownership checked before refresh can be skipped.

Attribution cache tests cover same-file priority, ambiguous names, method call sites, cross-file functions, function-only projection, the batched fallback and cache lifetime:

```sh
GOTOOLCHAIN=go1.27.2 go test -race -timeout=2m ./internal/resolver \
  -run '^TestIncrementalAttributionCache' -count=1
```

The cache seeds only changed-file nodes. Dataflow binding requests package functions only for eligible unresolved names without same-file candidates. It retains compact name/ID candidates for the active pass and clears them afterwards. Stores without the compound function finder retain one batched full-node fallback. Receiver, bare-name and generic attribution still run independently.

Run the public operation benchmark:

```sh
GOTOOLCHAIN=go1.27.2 GOMAXPROCS=4 go test ./internal/indexer \
  -run '^$' -bench '^BenchmarkIncrementalPublicOneFileSQLite$' \
  -benchtime=5x -count=3 -benchmem -timeout=15m
```

Fixture creation, cold indexing, two warm-up mutations and input writes are outside the benchmark timer. The timed operation is the public `IndexFile` call. Body edits change a call target; shape edits change a field declaration. Both preserve incoming callers in untouched files. No latency threshold is enforced in normal tests.

A local attribution-cache comparison on 2026-10-19 used Go 1.27.2, `GOMAXPROCS=4`, 512 files, 8706 nodes and 29698 edges. Medians of three runs with five updates each:

| Public mutation | Before | After | Before MB/op | After MB/op |
| --- | ---: | ---: | ---: | ---: |
| Body | 174.41 ms | 121.10 ms | 56.13 | 38.87 |
| Declaration | 167.57 ms | 126.61 ms | 55.48 | 38.21 |

MB/op is allocated bytes divided by 1,000,000, not retained process memory. A separate post-warmup CPU profile attributed about 19% of the before samples to eager sibling hydration; that path had no samples in the after profile on this workload. This does not imply zero cost or predict other repository shapes.

For per-family timings, run `BenchmarkIncrementalCoordinatorOneFileSQLite` with the same flags. This workload defers global passes during file catch-up and invokes the derived coordinator separately. It measures that pipeline, including resolver catch-up, rather than the complete public operation: affected-by, clone and semantic-provider work can differ. Do not compare its total directly with the public benchmark. The report preserves nanosecond durations; production logs expose each family in milliseconds, where zero can mean a sub-millisecond pass.

Capture CPU only after cold setup and warm-up:

```sh
artifact_dir=$(mktemp -d /tmp/gortex-incremental.XXXXXX)
GORTEX_INCREMENTAL_CPU_PROFILE="$artifact_dir/cpu" \
GOTOOLCHAIN=go1.27.2 GOMAXPROCS=4 go test ./internal/indexer \
  -run '^$' -bench '^BenchmarkIncrementalPublicOneFileSQLite/files=512/shape$' \
  -benchtime=20x -count=1 -benchmem -timeout=15m

go tool pprof -top "$artifact_dir/cpu.files512.shape.n20.pprof"
```

Choose one sub-benchmark and a fresh output prefix when collecting a profile. Existing profiles are never overwritten. The calibration run has its own `n1` profile. Sampling spans the measured iterations and their intervening fixture writes and assertions; it is not restricted to the benchmark timer. A plain `go test -cpuprofile` also includes cold fixture creation and can obscure the incremental path.

Keep the fixture size, namespace, binary, Go version, `GOMAXPROCS` and measurement mode identical for before/after comparisons. Repeat samples and retain logs. Shared-machine activity can affect results; these synthetic timings are not a production SLO and do not reproduce the historical 13.8-second derived pass by themselves.

Compare the scoped inference algorithm with the retained scan algorithm on identical SQLite data:

```sh
GOTOOLCHAIN=go1.27.2 GOMAXPROCS=4 go test ./internal/resolver \
  -run '^$' -bench '^BenchmarkScopedInferenceSQLitePaired$' \
  -benchtime=200ms -count=3 -benchmem -timeout=15m
```

These are warmed no-op inference passes, not complete file updates. The fixture includes unrelated repository data. A three-sample local run with `GOMAXPROCS=32`, 200 ms per sample and 2048 owners in each repository gave these medians:

| Pass | Repository scan | Exact frontier |
| --- | ---: | ---: |
| Implements, type only | 25.56 ms | 0.247 ms |
| Overrides, type only | 24.67 ms | 0.171 ms |
| Overrides, interface and type | 24.49 ms | 0.183 ms |

An interface in the affected frontier retains the original implements scan, with no claimed improvement for that case. Duplicate method names also retain the original override scan because generic adjacency reads do not promise the full projection's row order. Correctness takes priority over a faster ambiguous winner.

Existing SQL plan checks are useful alongside the workloads:

```sh
GOTOOLCHAIN=go1.27.2 go test -race -timeout=10m ./internal/graph/store_sqlite \
  -run '^(TestHotQueryPlansLocked|TestAdjacencyPlanLocksDuringBulkLoad|TestPreparedStatementPlansNeverScanBigTables|TestGenerationScopedReadPlansUseGenerationIndexes|TestScopedEdgeProjectionPlansStayInsideRequestedFiles)$' \
  -count=1
```

An indexed SQL lookup can still be expensive when repeated for unrelated data. Inspect the CPU profile and the selected frontier before adding an index. An interface change can legitimately affect implementing types throughout its repository; a type-only change and an override check should use their incident relationships instead of hydrating the repository's entire method graph.

# Working on Gortex

Gortex is a Go CLI, MCP server and daemon backed by a code graph. Keep changes focused and preserve existing working-tree edits. Start with Git status and the instructions relevant to the task; do not repeat a repository audit to resume known work.

## Working agreements

- Communicate with this maintainer in Russian. When proposing the next stage, recommend a model and reasoning effort before starting it. Choose by task complexity; do not automatically default to Astra Max. Give a short concrete plan, report actual verification, and finish with the next recommended step.
- Work autonomously through implementation and local verification. Use one agent for ordinary tasks; delegate only substantial independent work, with disjoint file ownership.
- Follow the active Gortex tooling instructions for indexed source. Scope queries to this repository and reuse established paths/evidence. Read only what the task needs.
- Never reset or clean the working tree, overwrite another session's edits, publish, or deploy without the corresponding request.
- For every release, update `version` in `cmd/gortex/main.go`. It is the source version used by local builds. Before tagging, verify that this value, the intended Git tag and the built binary's version agree. Preserve commit and dirty metadata so local changes remain identifiable.
- Development and load tests use temporary repositories, private XDG directories/SQLite storage, and isolated processes. Do not load the maintainer's real projects, restart the working daemon, delete its store, or re-track repositories to test a change.

## Where to work

- `cmd/gortex`: CLI commands and end-to-end daemon/proxy fixtures.
- `internal/mcp`: tool contracts, deadlines, mutation receipts and formatting.
- `internal/daemon`: socket protocol and client sessions.
- `internal/indexer`: incremental publication, watchers and reconciliation.
- `internal/graph/store_sqlite`: transactions, generations and graph/FTS persistence.
- `internal/embedding`: providers and shared model loading.
- `internal/testenv`, `internal/testutil`: reusable test isolation.

Use `.github/workflows/ci.yml` and `go.mod` for build/check conventions. The local installer defaults to the verified `go1.27.2` toolchain; override with `GOTOOLCHAIN` when deliberately testing another version. Go 1.27.2 needs a compatible golangci-lint (2.15.0 was verified); 2.13.1 cannot read its export data.

## Verification

Run focused tests first. Broaden once when the affected contracts justify it, and repeat only for new changes, failures or unresolved risks. Save long-run output to a temporary log so a failure is not lost to terminal truncation.

```sh
go build ./cmd/gortex
go test -race -timeout=15m ./internal/mcp
go test -race -timeout=25m ./internal/indexer ./internal/graph/store_sqlite
go test -race -timeout=10m ./cmd/gortex
golangci-lint run --timeout=10m
go mod tidy -diff
```

Choose the affected package rather than running every command for every edit. Full CI uses `go test -race -timeout=45m -coverprofile=coverage.out ./...`; SQLite and indexer can each take about 10 to 12 minutes under race. Report interrupted or failed runs honestly. Do not weaken checks or add suppressions to obtain a pass. Before every push, complete the applicable lint, security checks and tests.

For real MCP transport load, run `bash scripts/test-agent-transport.sh`; see [the scenario guide](bench/agent-transport/README.md) for parameters, profiles and limits. It requires no LLM or external model service. Installer changes have fast contract tests: `bash scripts/test-install-local.sh` (also part of the normal Go suite).

## Contracts to preserve

- Graph test links are not measured execution coverage. `.gortex.yaml` excludes `**/*_test.go`; missing test nodes mean unavailable evidence, not proof of no tests or zero coverage.
- SQLite file publication must keep graph nodes, incoming references and symbol FTS consistent. Failed publication retains prior state and cannot advance content, sidecars or freshness receipts. New optional store capabilities belong in the generation checklist and its real write-fence tests.
- Shared model loading must not hold the cache mutex during I/O. Cancellation belongs to each waiter; cancel the load when its last waiter leaves. Use local HTTP servers for failure tests.
- A timed-out mutation may have committed. Check its ledger with the original stable transaction ID before deciding what to do. Retry only an explicit undelivered admission refusal; do not retry a semantic miss or an uncertain mutation blindly.
- Filesystem locks coordinate Gortex operations, not arbitrary external processes. Do not claim complete external TOCTOU protection.

## Local installation

`bash scripts/install-local.sh` builds the current checkout, including dirty source, and atomically installs into `~/.local/bin`, retaining `gortex.previous`. `--rollback` swaps the last two binaries. It does not restart the daemon or change configuration.

Test installation with a disposable `GORTEX_INSTALL_DIR`, never the maintainer's real executable. `--dry-run` builds/validates without changing the destination; `--binary PATH` installs a trusted prebuilt binary. The existing `scripts/install.sh` downloads a published fork release and may restart the daemon, so it is not the installer for unpublished local fixes.

# Local Gortex observability

The daemon continuously records lightweight metrics. The embedded dashboard and Prometheus HTTP listener are disabled by default, so no dashboard port is occupied. Enable them explicitly for a foreground daemon:

```sh
gortex daemon start --metrics-addr 127.0.0.1:9474
```

To control an already running daemon without restarting it:

```sh
gortex daemon dashboard start
gortex daemon dashboard status
gortex daemon dashboard stop
```

`start` prints the browser URL and reuses the startup address, or chooses
`127.0.0.1:9474` when none was configured. Use `start --address 127.0.0.1:9476`
to choose another loopback port. Repeated start/stop calls are safe; stop before
changing the address of a running dashboard. Add `--json` for machine-readable
state. These commands change runtime state only. Stopping the dashboard closes
its HTTP connections and releases the port; metric collection and history continue.

For persistent daemon startup, set this in `~/.gortex/config.yaml`:

```yaml
daemon:
  metrics_addr: "127.0.0.1:9474"
```

Then open http://127.0.0.1:9474. No containers, exporter, cloud account or paid service are required. Prometheus, Grafana and Tempo remain an optional companion stack for additional queries and sampled traces.

The stack never starts, configures or restarts Gortex. Use a build containing the observability changes before enabling its endpoints. Older releases do not provide these metrics.

## Built-in interface and daily history

Metrics are sampled every 10 seconds, whether or not a browser or Prometheus is connected. The embedded interface shows live CPU, memory, MCP traffic and latency, readiness and cached repository metadata. It supports 15-minute, one-hour, six-hour and 24-hour ranges; pausing the display does not pause collection. Unknown data stays unavailable. Counter rates break at process restarts and long gaps.

History is stored separately from the graph in `observability/history.sqlite` under the daemon's cache directory. It survives restart and serves no point older than 24 hours. Startup, reads, writes and the periodic collector expire old records. While the daemon is stopped no cleanup runs; startup removes expired records before serving history. Up to 8640 points and 32 MiB of compressed payload are retained, so high cardinality can shorten the available window. The SQLite file has a separate bounded page budget and rollback-journal overhead; this is not a claim that total disk usage is exactly 32 MiB. No request arguments, source text, credentials or raw error messages are recorded. Daily MCP charts aggregate operations; the Prometheus endpoint retains per-operation counters.

Listener precedence is `--metrics-addr`, then `GORTEX_METRICS_ADDR`, then `daemon.metrics_addr`. With all three unset, HTTP stays disabled while collection and history continue. An explicit empty flag/environment value or `off` disables HTTP even when a lower-priority source enables it. An explicitly configured invalid or occupied address fails startup. HTTP remains loopback-only with Host/Origin checks. A storage failure leaves live metrics running and is reported as unavailable history.

Read-only GET endpoints are `/api/v1/current` (latest sample with Unix-millisecond `at` and canonical metric `values`) and `/api/v1/history?seconds=3600&limit=360` (samples, availability and 86400-second retention). History accepts `seconds` from 1 to 86400, `limit` from 2 to 1440, and optional `scope=charts` to omit repository metadata from frequent chart refreshes. Responses and concurrent readers are bounded; errors never expose filesystem paths or SQL. `/metrics` remains the Prometheus endpoint. Expensive profiles and exported traces are still opt-in.

Build and activate these changes when agent work permits:

```sh
bash scripts/install-local.sh
"$HOME/.local/bin/gortex" daemon restart
```

Restart interrupts active MCP connections. Development checks use private homes, stores and disposable daemons instead of changing the working installation. The published v1.1.0 predates the built-in daily history, so downloading that release does not install these source changes.

## Optional Grafana interface

Requirements: native Linux, Docker Engine with Compose, Python 3, and available loopback ports 3000 and 9090. Tracing also needs 3200, 4318 and 9095.

From the repository root:

```sh
sh deploy/observability/stack.sh up
```

Open [Grafana](http://127.0.0.1:3000), username `admin`. Retrieve the generated password locally:

```sh
cat deploy/observability/.state/grafana-password
```

The password is generated once, never printed by the launcher, and excluded from Git. Its parent directory is private. The file is readable inside Grafana's unprivileged container through a Compose secret. Docker administrators can access container data and secrets.

Grafana provisions four dashboards in the **Gortex** folder:

- **Runtime and readiness:** scrape health, graph readiness, enrichment, warmup, daemon CPU/RSS/PSS, child RSS, Go heap/GC/goroutines, sessions, cache memory and I/O.
- **MCP requests:** completed call rate/outcomes, p50/p95/p99 latency, operation breakdown, in-flight calls and timed-out handlers still running.
- **Repository snapshots:** cached per-repository files/nodes/edges/indexed state, aggregate availability, source and snapshot age.
- **SQLite:** connection pools/limits/admission waits and asynchronously sampled database/WAL sizes with availability and sample age. Pool admission waits do not measure query duration or SQLite lock contention.

Missing data stays missing. It is not converted to zero. Repository counts are cached snapshots, not progress percentages or queue lengths. An autonomous bounded metadata sampler refreshes the aggregate independently of CLI calls; the dashboard displays its source (`status` or `indexer_metadata`) and age. Advisory metadata alone does not assert that a repository is indexed: loaded-state series are omitted until that state is observed through a status snapshot. Scrapes do not scan the graph. Use `gortex daemon monitor` to follow live startup phases, also recorded in the runtime dashboard's phase timeline. An old aggregate can remain visible; check its age.

[Prometheus](http://127.0.0.1:9090/targets) shows whether the `gortex` target is reachable. Starting the stack before enabling daemon metrics produces an expected DOWN target.

## Configure the native daemon endpoint

Metrics collection and the local dashboard are enabled by default and listen separately from profiling. No service override is needed for the default address. To choose an address explicitly for an existing systemd user service:

```sh
systemctl --user edit com.zzet.gortex.service
```

Add:

```ini
[Service]
Environment=GORTEX_METRICS_ADDR=127.0.0.1:9474
```

Apply it when you are ready to restart your daemon:

```sh
systemctl --user daemon-reload
systemctl --user restart com.zzet.gortex.service
```

This interrupts existing connections and starts a new warmup. The commands above are manual setup, not actions performed by the stack. Keep the existing binary, arguments and data directories in the service. Do not start a second daemon.

For a foreground test daemon, use `gortex daemon start --metrics-addr 127.0.0.1:9474`. CLI flags configure that invocation; they do not persist service settings. The scrape target is `/metrics` on port 9474. If you choose another port, update `prometheus.yaml` as well.

## Optional traces

```sh
sh deploy/observability/stack.sh up --tracing
```

Add another service environment setting:

```ini
Environment=OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:4318/v1/traces
```

Apply the service change manually as above. In Grafana, open **Explore**, select **Gortex Tempo (optional)**, and search for service `gortex`. That datasource is provisioned even when tracing is disabled; its connection error in that case is expected.

Gortex exports OTLP over HTTP. Sampling defaults to parent-based 10%; use `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` for an explicit sampling policy. `OTEL_TRACES_EXPORTER=none` disables export. Export uses a bounded queue and timeout; a failed collector must not block MCP calls. Use sampling deliberately: tracing every operation increases overhead and disk consumption.

Tempo retains trace blocks for 24 hours. Retention is periodic cleanup, not an exact disk quota. Trace and repository metadata may reveal project names and operation patterns, so keep the volumes private.

## Profiles

Profiling has a separate optional endpoint. Add `Environment=GORTEX_DIAGNOSTICS_ADDR=127.0.0.1:9475` to the same service override and apply it manually. A bounded CPU capture then uses:

```sh
gortex daemon profile --kind cpu --duration 10s --out cpu.pprof
go tool pprof cpu.pprof
```

The compatibility variable `GORTEX_DAEMON_PPROF_ADDR` also enables this managed listener when the new address is unset. It does not start a second profile server. The old argv endpoint is not exposed. CPU captures are limited to 60 seconds; runtime trace, block and mutex captures to 30 seconds. Every artifact is limited to 32 MiB. Expensive captures are mutually exclusive; one bounded goroutine snapshot remains available during a CPU capture, including text dumps with `?debug=2`. Disconnecting cancels a running timed capture. Block/mutex profiles are cumulative; their sampling is enabled only during the requested interval. Files saved by `daemon profile` use mode 0600 and existing files are preserved.

Use `gortex daemon monitor`, `--details`, `--plain`, `--json` or `--once` for a local view without containers. In the interactive view, `p` pauses polling, Enter or `d` toggles repository details, and `q` exits the monitor. It does not stop the daemon. Actual file-level progress is marked unavailable when the backend has no corresponding measurements.

Profiles are local artifacts; Grafana does not ingest them automatically. Keep diagnostic endpoints on loopback and collect profiles only while investigating. Go runtime memory counters, process RSS and proportional memory describe different things. Summing child RSS can double-count shared pages. Host-wide page cache is not attributed to Gortex as if it were its private memory.

## Operation and limits

```sh
sh deploy/observability/stack.sh status
sh deploy/observability/stack.sh logs prometheus
sh deploy/observability/stack.sh down
```

`down` preserves named volumes and the password. Stop and start the stack to apply configuration changes. Changing the initial password file does not reset an existing Grafana account; use Grafana's password controls.

Defaults: Prometheus is configured for one day or approximately 2 GB of TSDB blocks, whichever retention threshold is reached first. Prometheus removes whole blocks asynchronously; this is not an exact per-record deletion deadline. WAL/head data needs additional disk space. Prometheus and Grafana each have a 512 MiB memory limit; optional Tempo has 1 GiB. Each container is limited to one CPU. Logs rotate at 10 MB with two files. These are starting limits for a workstation, not guarantees about workload capacity; inspect collector logs for OOMs or rejected scrapes before increasing them.

Images are pinned to Prometheus 3.15.0, Grafana 13.2.4 and Tempo 3.1.0. Review their release/security notes and rerun the smoke test when updating pins. No unpinned plugin downloads are needed.

All services use native Linux host networking so Prometheus can reach a loopback-only daemon without opening its diagnostics to the LAN. Every server listener is explicitly bound to `127.0.0.1`. This includes Tempo's HTTP, internal gRPC and OTLP receiver. Host networking shares the host network namespace; there are no published port mappings, Docker socket mounts, privileged containers or host PID access. Local machine users can still reach unauthenticated Prometheus/Tempo listeners. Do not expose them with a wildcard bind.

Docker Desktop runs its engine in a VM. Its host networking and host-loopback behavior differ, so this manifest is not a portable Desktop setup. Rootless Podman also changes host-loopback and file ownership behavior. It has not been validated here; use native Linux Docker or adapt and test the network/volume settings explicitly. Adding `host.docker.internal` alone cannot make a host loopback-only service reachable through a bridge.

## Validate without touching a working daemon

```sh
sh deploy/observability/stack.sh check
sh deploy/observability/stack.sh smoke
```

`check` validates Compose, runs the pinned image's `promtool check config`, checks the provisioned dashboard contracts, and runs fast tests for loopback-only scrape targets and redirect refusal. Docker may download missing pinned images.

`smoke` creates a unique Compose project, random loopback ports, disposable volumes, a local fake metrics endpoint, and a synthetic trace. It checks a real Prometheus scrape, Grafana dashboard provisioning, all dashboard PromQL syntax, OTLP ingestion and trace lookup. It stops the test containers and deletes only its own volumes. By default it does not query a real daemon or load any repositories. For integration with an already running **isolated test daemon**, set `GORTEX_OBSERVABILITY_SMOKE_METRICS_URL=http://127.0.0.1:PORT/metrics`. Only that literal loopback URL format is accepted. Wait for ready and execute at least one MCP tool call first, so request histogram series exist. The smoke then scrapes that test endpoint and checks required actual metric sample names; all monitoring containers and ports remain disposable. Never point this override at the maintainer's working daemon.

Design references: [Prometheus naming](https://prometheus.io/docs/practices/naming/), [Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/), [Tempo local deployment](https://grafana.com/docs/tempo/latest/set-up-for-tracing/setup-tempo/deploy/locally/), [Docker host networking](https://docs.docker.com/engine/network/drivers/host/).

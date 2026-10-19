# Local Gortex observability

Prometheus records daemon metrics. Grafana provides the dashboards. Tempo stores optional sampled traces. Gortex remains a native Linux process; these containers are an optional companion application. No exporter, cloud account or paid service is required.

The stack never starts, configures or restarts Gortex. Use a build containing the observability changes before enabling its endpoints. Older releases do not provide these metrics.

## Start the interface

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

## Enable metrics in the native daemon

Metrics are opt-in and listen separately from profiling. For an existing systemd user service:

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

Defaults: Prometheus keeps up to 7 days or approximately 2 GB of TSDB blocks, whichever retention threshold is reached first. WAL/head data needs additional disk space. Prometheus and Grafana each have a 512 MiB memory limit; optional Tempo has 1 GiB. Each container is limited to one CPU. Logs rotate at 10 MB with two files. These are starting limits for a workstation, not guarantees about workload capacity; inspect collector logs for OOMs or rejected scrapes before increasing them.

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

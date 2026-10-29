# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

The developer who runs the Gortex daemon on their own machine for several coding agents at once (Claude Code, Codex and others) across many tracked repositories. They open the local dashboard when an agent reports a problem, or to check that the daemon is healthy, and they need the answer without reading logs.

## Product Purpose

Gortex indexes repositories into a code graph and serves it to agents over MCP and the CLI. The local dashboard exists to answer two questions first: why are agents reporting problems right now (slow or timed-out tools, indexing lag, edits not reaching the graph, unhealthy repositories), and is the daemon healthy overall (readiness, CPU, memory, storage, trends over the last 24 hours). Success is that the developer can name the cause of a complaint from the first screen.

## Operating Context

- The dashboard is served by the running daemon on a loopback address (default `127.0.0.1:9474`), started and stopped at runtime with `gortex daemon dashboard start|stop` without restarting the daemon.
- Metric collection and a rolling 24-hour history run whether or not the dashboard is open; `/metrics` serves the same data for Prometheus.
- A typical machine tracks about a hundred repositories in one store of tens of gigabytes; several agent sessions issue MCP requests concurrently.

## Capabilities and Constraints

- Strict Content-Security-Policy: only same-origin scripts, styles, data and images. No external fonts, CDNs or third-party libraries; everything ships embedded in the Go binary.
- The dashboard handler admits one request at a time and answers `dashboard busy` otherwise, so the page must request data sequentially and tolerate a busy answer.
- Data comes from `/api/v1/current` (latest sample) and `/api/v1/history` (bounded 24-hour samples); missing data must stay visibly unknown rather than shown as zero.
- Required data: per-tool latency and timeouts, indexing and edit application, storage and cache, plus CPU, memory and MCP throughput. Indexing and edit metrics do not exist yet and must be added to the daemon.
- Interface language: English, like the CLI, docs and daemon messages.

## Brand Commitments

The fork identifies itself as "Gortex · fork by kordax". No other visual identity is committed.

## Evidence on Hand

Live metric families are listed by `GET /metrics` on a running daemon. No screenshots, customers or benchmarks are to be invented.

## Product Principles

1. Diagnose first: lead with what is wrong and where, then with general health.
2. Unknown is not zero: a missing or stale value says so.
3. Never disturb the daemon: viewing is read-only, bounded and cheap.
4. Every number names its unit and its source.

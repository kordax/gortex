package main

import (
	"context"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/observability"
)

// daemonMetricsHandler never calls Status or performs graph/database scans.
// The aggregate is an explicitly timestamped snapshot from ordinary status
// collection; unknown data is omitted rather than manufactured as zero.
func daemonMetricsHandler(ctx context.Context, srv *daemon.Server, c *realController) http.Handler {
	started := time.Now()
	var metadata atomic.Pointer[statusAggregate]
	var files atomic.Pointer[daemonMetricFiles]
	if c != nil {
		go sampleDaemonMetricMetadata(ctx, c, &metadata)
		go sampleDaemonMetricFiles(ctx, c.graph, &files)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil {
			http.Error(w, "daemon stopping", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		e := observability.NewExposition(w)
		gauge := func(name, help string, v float64, labels ...string) { e.Gauge("gortex_"+name, help, v, labels...) }
		counter := func(name, help string, v float64, labels ...string) { e.Counter("gortex_"+name, help, v, labels...) }
		flag := func(v bool) float64 {
			if v {
				return 1
			}
			return 0
		}
		gauge("daemon_uptime_seconds", "Seconds since observability initialization.", time.Since(started).Seconds())
		if c != nil {
			gauge("daemon_ready", "One when reference resolution is complete and queries are ready.", flag(c.ready.Load()))
			gauge("daemon_enrichment_complete", "One after background enrichment has completed.", flag(c.enriched.Load()))
			if c.ready.Load() {
				gauge("daemon_warmup_duration_seconds", "Completed reference warmup duration.", float64(c.warmupSeconds.Load()))
			}
			agg := c.lastAggregate.Load()
			source := "status"
			if sampled := metadata.Load(); sampled != nil && (agg == nil || sampled.takenAt.After(agg.takenAt)) {
				agg = sampled
				source = "indexer_metadata"
			}
			if agg != nil {
				gauge("daemon_aggregate_source", "One for the current repository counter source; metadata counts are advisory.", 1, "source", source)
			}
			gauge("daemon_aggregate_available", "One when a status aggregate has been collected.", flag(agg != nil))
			if agg != nil {
				gauge("daemon_aggregate_snapshot_timestamp_seconds", "Unix timestamp of cached repository counts.", float64(agg.takenAt.Unix()))
				counts := map[string]int{"indexed": 0, "unloaded": 0, "missing": 0, "metadata": 0}
				for _, repo := range agg.tracked {
					state := "indexed"
					if source == "indexer_metadata" {
						state = "metadata"
					}
					if repo.Unloaded {
						state = "unloaded"
					}
					if repo.Missing {
						state = "missing"
					}
					counts[state]++
					if !metricRepoName(repo.Prefix) {
						continue
					}
					labels := []string{"repo", repo.Prefix}
					gauge("repo_nodes", "Nodes from the timestamped repository status snapshot.", float64(repo.Nodes), labels...)
					gauge("repo_edges", "Edges from the timestamped repository status snapshot.", float64(repo.Edges), labels...)
					gauge("repo_files", "Files from the timestamped repository status snapshot.", float64(repo.Files), labels...)
					if source == "status" {
						gauge("repo_indexed", "One when this repository has a loaded index in the status snapshot.", flag(!repo.Unloaded && !repo.Missing), labels...)
					}
				}
				states := []string{"indexed", "unloaded", "missing"}
				if source == "indexer_metadata" {
					states = []string{"metadata"}
				}
				for _, state := range states {
					gauge("daemon_repos", "Repositories by observed status; metadata does not imply a queryable graph.", float64(counts[state]), "state", state)
				}
				if p := agg.enrichment; p != nil {
					gauge("enrichment_repos", "Repository enrichment pass counts from the cached aggregate.", float64(p.ReposTotal), "state", "total")
					gauge("enrichment_repos", "Repository enrichment pass counts from the cached aggregate.", float64(p.ReposDone), "state", "done")
				}
			}
		}
		if srv != nil {
			if srv.Ready != nil {
				_, phase := srv.Ready()
				if !metricRepoName(phase) || len(phase) > 48 {
					phase = "unknown"
				}
				gauge("daemon_phase", "One for the daemon's current readiness phase.", 1, "phase", phase)
			}
			socket, retained, transport := srv.SessionCounts()
			gauge("daemon_sessions", "Registered sessions by connection state.", float64(socket), "state", "socket")
			gauge("daemon_sessions", "Registered sessions by connection state.", float64(retained), "state", "retained")
			gauge("daemon_sessions", "Registered sessions by connection state.", float64(transport), "state", "transport")
		}
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		gauge("runtime_heap_alloc_bytes", "Live Go heap allocations, distinct from process RSS.", float64(mem.Alloc))
		gauge("runtime_heap_inuse_bytes", "Go heap spans in use.", float64(mem.HeapInuse))
		gauge("runtime_heap_idle_bytes", "Idle Go heap spans including released memory.", float64(mem.HeapIdle))
		gauge("runtime_heap_released_bytes", "Go heap memory returned to the OS.", float64(mem.HeapReleased))
		gauge("runtime_sys_bytes", "Go runtime reserved memory, not process RSS.", float64(mem.Sys))
		gauge("runtime_stack_inuse_bytes", "Go goroutine stack spans in use.", float64(mem.StackInuse))
		gauge("runtime_goroutines", "Current Go goroutines.", float64(runtime.NumGoroutine()))
		counter("runtime_gc_cycles_total", "Completed Go garbage collections.", float64(mem.NumGC))
		if cache := trigramCacheForResponse(); cache != nil {
			gauge("cache_entries", "Live cached repository indexes.", float64(cache.Live), "cache", "trigram")
			gauge("cache_bytes", "Estimated live cache heap bytes.", float64(cache.Bytes), "cache", "trigram")
			gauge("cache_limit_bytes", "Configured cache heap byte budget.", float64(cache.MaxBytes), "cache", "trigram")
			counter("cache_evictions_total", "Cache entries evicted since process start.", float64(cache.Evictions), "cache", "trigram")
		}
		p := observability.Process()
		gauge("process_sample_available", "One when Linux process counters are available.", flag(p.Available))
		gauge("process_sample_timestamp_seconds", "Unix timestamp of the asynchronous OS process sample.", float64(p.SampleUnix))
		if p.Available {
			counter("process_cpu_seconds_total", "Daemon user plus system CPU seconds.", p.CPUSeconds)
			gauge("process_resident_memory_bytes", "Daemon resident memory including shared pages.", float64(p.RSSBytes))
			gauge("process_pss_available", "One when proportional-set size was readable.", flag(p.PSSAvailable))
			if p.PSSAvailable {
				gauge("process_pss_memory_bytes", "Daemon proportional-set memory from smaps_rollup.", float64(p.PSSBytes))
			}
			gauge("process_io_available", "One when Linux process I/O counters were readable.", flag(p.IOAvailable))
			if p.IOAvailable {
				counter("process_io_bytes_total", "Daemon storage I/O bytes from procfs, excluding page-cache hits.", float64(p.ReadBytes), "direction", "read")
				counter("process_io_bytes_total", "Daemon storage I/O bytes from procfs, excluding page-cache hits.", float64(p.WriteBytes), "direction", "write")
			}
			gauge("child_processes", "Current descendant processes; excludes unrelated MCP clients.", float64(p.Children))
			gauge("children_resident_memory_bytes", "Sum of descendant RSS; shared pages can be counted repeatedly.", float64(p.ChildrenRSSBytes))
			gauge("children_cpu_seconds", "Accumulated CPU of current descendants; decreases when children exit.", p.ChildrenCPUSeconds)
			gauge("children_sample_complete", "One when no descendant sampling errors were observed; procfs is non-atomic.", flag(p.ChildrenComplete))
		}
		if c != nil {
			writeDaemonSQLMetrics(e, c.graph, files.Load())
		}
		observability.Default.Write(e)
		observability.Indexing.Write(e)
		if c != nil {
			writeWatcherStateMetrics(e, c.watcher())
		}
		gauge("mcp_abandoned_handlers", "Timed-out or cancelled MCP handlers still running in the background.", float64(mcp.AbandonedToolCalls()))
	})
}

func metricRepoName(name string) bool {
	if len(name) == 0 || len(name) > 128 || strings.ContainsAny(name, "/\\\n\r") {
		return false
	}
	return true
}

// writeWatcherStateMetrics emits one series per repository naming its watcher
// condition, so a repository whose edits stop reaching the graph is visible
// without reading the log.
func writeWatcherStateMetrics(e *observability.Exposition, mw *indexer.MultiWatcher) {
	if mw == nil {
		return
	}
	states := mw.WatcherStates()
	repos := make([]string, 0, len(states))
	for repo := range states {
		if metricRepoName(repo) {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	for _, repo := range repos {
		e.Gauge("gortex_watcher_state", "One for each repository's current file watcher condition.", 1, "repo", repo, "state", states[repo])
	}
}

package observability

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Edits that reach the graph late are the cause behind most "stale name" and
// "tool timed out" reports, and nothing measured them: the watcher logged each
// patch and moved on. These series record how long a change takes to land, how
// often it waits for a mutation slot, and how bursts drain, per repository.
var indexingBuckets = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// maxIndexingRepos bounds the repository label. A workspace past it folds the
// remainder into "other" rather than growing the exposition without limit.
const maxIndexingRepos = 512

type histogram struct {
	buckets [len(indexingBuckets)]uint64
	count   uint64
	sum     float64
}

func (h *histogram) observe(seconds float64) {
	h.count++
	h.sum += seconds
	for i, bound := range indexingBuckets {
		if seconds <= bound {
			h.buckets[i]++
		}
	}
}

type repoIndexing struct {
	patches      uint64
	patchSeconds float64
	lastPatch    time.Time
	deferred     uint64
	drains       uint64
	drainPaths   uint64
	drainFailed  uint64
	drainSeconds float64
}

// IndexingRegistry holds the edit-to-graph measurements. The zero value is
// ready to use.
type IndexingRegistry struct {
	mu        sync.Mutex
	repos     map[string]*repoIndexing
	patch     histogram
	drain     histogram
	derived   histogram
	framework float64
}

// Indexing is the process-wide registry the watcher and indexer report to.
var Indexing = &IndexingRegistry{}

func indexingRepoLabel(repo string) string {
	if repo == "" {
		return "default"
	}
	if len(repo) > 128 || strings.ContainsAny(repo, "/\\\n\r\"") {
		return "other"
	}
	return repo
}

func (r *IndexingRegistry) repoLocked(repo string) *repoIndexing {
	label := indexingRepoLabel(repo)
	if r.repos == nil {
		r.repos = make(map[string]*repoIndexing)
	}
	st := r.repos[label]
	if st == nil {
		if len(r.repos) >= maxIndexingRepos {
			label = "other"
			st = r.repos[label]
		}
		if st == nil {
			st = &repoIndexing{}
			r.repos[label] = st
		}
	}
	return st
}

// ObservePatch records one file change applied to the graph.
func (r *IndexingRegistry) ObservePatch(repo string, elapsed time.Duration) {
	seconds := elapsed.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.repoLocked(repo)
	st.patches++
	st.patchSeconds += seconds
	st.lastPatch = time.Now()
	r.patch.observe(seconds)
}

// NoteDeferredPatch records a change that gave up waiting for a mutation slot
// and was rescheduled.
func (r *IndexingRegistry) NoteDeferredPatch(repo string) {
	r.mu.Lock()
	r.repoLocked(repo).deferred++
	r.mu.Unlock()
}

// ObserveStormDrain records one burst of changes reindexed together.
func (r *IndexingRegistry) ObserveStormDrain(repo string, paths, failed int, elapsed time.Duration) {
	seconds := elapsed.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.repoLocked(repo)
	st.drains++
	st.drainPaths += uint64(max(paths, 0))
	st.drainFailed += uint64(max(failed, 0))
	st.drainSeconds += seconds
	st.lastPatch = time.Now()
	r.drain.observe(seconds)
}

// ObserveDerivedPasses records the derived graph passes that follow a change,
// and the part of them spent in framework synthesis.
func (r *IndexingRegistry) ObserveDerivedPasses(total, framework time.Duration) {
	r.mu.Lock()
	r.derived.observe(total.Seconds())
	r.framework += framework.Seconds()
	r.mu.Unlock()
}

type repoIndexingRow struct {
	repo string
	repoIndexing
}

// Write emits the indexing families.
func (r *IndexingRegistry) Write(e *Exposition) {
	r.mu.Lock()
	rows := make([]repoIndexingRow, 0, len(r.repos))
	for repo, st := range r.repos {
		rows = append(rows, repoIndexingRow{repo: repo, repoIndexing: *st})
	}
	patch, drain, derived, framework := r.patch, r.drain, r.derived, r.framework
	r.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].repo < rows[j].repo })

	writeIndexingHistogram(e, "gortex_index_patch_duration_seconds", "Time to apply one changed file to the graph.", patch)
	writeIndexingHistogram(e, "gortex_index_storm_drain_duration_seconds", "Time to reindex one burst of changed files.", drain)
	writeIndexingHistogram(e, "gortex_index_derived_duration_seconds", "Derived graph passes that follow a change.", derived)
	e.Counter("gortex_index_derived_framework_seconds_total", "Part of the derived passes spent in framework synthesis.", framework)
	for _, row := range rows {
		repo := []string{"repo", row.repo}
		e.Counter("gortex_index_patches_total", "Changed files applied to the graph.", float64(row.patches), repo...)
		e.Counter("gortex_index_patch_seconds_total", "Seconds spent applying changed files to the graph.", row.patchSeconds, repo...)
		e.Counter("gortex_index_deferred_patches_total", "Changes that waited out a mutation slot and were rescheduled.", float64(row.deferred), repo...)
		e.Counter("gortex_index_storm_drains_total", "Bursts of changed files reindexed together.", float64(row.drains), repo...)
		e.Counter("gortex_index_storm_drain_paths_total", "Paths reindexed by burst drains.", float64(row.drainPaths), repo...)
		e.Counter("gortex_index_storm_drain_failed_paths_total", "Paths a burst drain failed to reindex.", float64(row.drainFailed), repo...)
		e.Counter("gortex_index_storm_drain_seconds_total", "Seconds spent draining bursts of changed files.", row.drainSeconds, repo...)
		if !row.lastPatch.IsZero() {
			e.Gauge("gortex_index_last_change_timestamp_seconds", "Unix time the last change reached the graph.", float64(row.lastPatch.Unix()), repo...)
		}
	}
}

func writeIndexingHistogram(e *Exposition, name, help string, h histogram) {
	e.family(name, help, "histogram")
	for i, bound := range indexingBuckets {
		e.sample(name+"_bucket", float64(h.buckets[i]), "le", strconv.FormatFloat(bound, 'g', -1, 64))
	}
	e.sample(name+"_bucket", float64(h.count), "le", "+Inf")
	e.sample(name+"_sum", h.sum)
	e.sample(name+"_count", float64(h.count))
}

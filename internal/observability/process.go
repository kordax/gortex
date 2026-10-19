package observability

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// ProcessSnapshot reports only this daemon and its current descendants. Child
// RSS is a sum, so shared pages can be counted more than once. It excludes MCP
// clients and proxies that are not descendants. Unknown values have flags.
type ProcessSnapshot struct {
	Available          bool    `json:"available"`
	PID                int     `json:"pid"`
	StartTicks         uint64  `json:"start_ticks"`
	CPUSeconds         float64 `json:"cpu_seconds"`
	RSSBytes           uint64  `json:"rss_bytes"`
	PSSBytes           uint64  `json:"pss_bytes"`
	PSSAvailable       bool    `json:"pss_available"`
	ReadBytes          uint64  `json:"read_bytes"`
	WriteBytes         uint64  `json:"write_bytes"`
	IOAvailable        bool    `json:"io_available"`
	Children           int     `json:"children"`
	ChildrenRSSBytes   uint64  `json:"children_rss_bytes"`
	ChildrenCPUSeconds float64 `json:"children_cpu_seconds"`
	ChildrenComplete   bool    `json:"children_complete"`
	SampleUnix         int64   `json:"sample_unix"`
}

var processCache atomic.Pointer[ProcessSnapshot]
var processRefreshing atomic.Bool

// Process returns a copy of the cached sample immediately. It never waits for
// procfs, smaps or filesystem I/O on a control request or Prometheus scrape.
// Refreshes are single-flight and demand driven, with no permanent goroutine.
func Process() *ProcessSnapshot {
	cached := processCache.Load()
	if cached == nil || time.Now().Unix()-cached.SampleUnix >= 2 {
		if processRefreshing.CompareAndSwap(false, true) {
			go func() {
				defer processRefreshing.Store(false)
				sample := collectProcess("/proc", os.Getpid(), runtime.GOOS)
				sample.SampleUnix = time.Now().Unix()
				processCache.Store(&sample)
			}()
		}
	}
	if cached == nil {
		return &ProcessSnapshot{PID: os.Getpid()}
	}
	copy := *cached
	return &copy
}

package mcp

import "time"

// analysisRequestLockWait bounds how long a request waits out a short
// analysisMu writer (a materialization install, a hotspot publish) before it
// answers without analysis data.
const analysisRequestLockWait = 250 * time.Millisecond

// beginAnalysisRecompute marks a whole-graph analysis computation that is about
// to hold analysisMu for writing. It must be called before the lock is
// requested and its release after the lock is dropped.
//
// RunAnalysis holds the write lock for the whole Leiden, PageRank, HITS and
// concept pass, which takes minutes on a large workspace. Request readers that
// queued behind it sat until their tool deadline, so a natural-language search
// on a busy daemon timed out instead of answering. While the mark is set they
// answer without analysis data instead. They lose nothing by it: a pass only
// starts after the graph moved, so the snapshot they would wait for is stale
// until the pass publishes, and every getter already answers nil for a stale
// snapshot.
func (s *Server) beginAnalysisRecompute() func() {
	s.analysisRecomputes.Add(1)
	return func() { s.analysisRecomputes.Add(-1) }
}

// rlockAnalysisForRequest read-locks analysisMu for a request path. It reports
// false, without the lock, when a recompute holds or awaits the lock, or when a
// short writer does not finish within analysisRequestLockWait. The caller
// treats false like a stale snapshot.
func (s *Server) rlockAnalysisForRequest() bool {
	return s.acquireAnalysisForRequest(s.analysisMu.TryRLock)
}

// lockAnalysisForRequest is rlockAnalysisForRequest for the request paths that
// publish into the analysis state.
func (s *Server) lockAnalysisForRequest() bool {
	return s.acquireAnalysisForRequest(s.analysisMu.TryLock)
}

func (s *Server) acquireAnalysisForRequest(try func() bool) bool {
	deadline := time.Now().Add(analysisRequestLockWait)
	for {
		// The mark is set before a recompute requests the lock, so a failed try
		// that races its start sees the mark on the next check.
		if s.analysisRecomputes.Load() > 0 {
			return false
		}
		if try() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

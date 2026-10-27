package indexer

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestIdlePollDelayBacksOffToCap(t *testing.T) {
	const base = 15 * time.Second
	for _, tc := range []struct {
		name string
		base time.Duration
		idle time.Duration
		want time.Duration
	}{
		{"active", base, 0, base},
		{"just_before_idle", base, checkoutIdlePollAfter - time.Nanosecond, base},
		{"first_idle_period", base, checkoutIdlePollAfter, 2 * base},
		{"second_idle_period", base, 2 * checkoutIdlePollAfter, 4 * base},
		{"fourth_idle_period", base, 4 * checkoutIdlePollAfter, 16 * base},
		{"capped", base, 5 * checkoutIdlePollAfter, checkoutIdlePollMax},
		{"long_idle_capped", base, 100 * time.Hour, checkoutIdlePollMax},
		{"disabled", -1, time.Hour, -1},
		{"zero", 0, time.Hour, 0},
		{"base_above_cap", 10 * time.Minute, time.Hour, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := idlePollDelay(tc.base, tc.idle); got != tc.want {
				t.Fatalf("idlePollDelay(%s, %s) = %s, want %s", tc.base, tc.idle, got, tc.want)
			}
		})
	}
}

// cycleRecorder counts cycles and keeps their fake-clock times.
type cycleRecorder struct {
	mu      sync.Mutex
	started time.Time
	at      []time.Duration
}

func (r *cycleRecorder) record() {
	r.mu.Lock()
	r.at = append(r.at, time.Since(r.started))
	r.mu.Unlock()
}

func (r *cycleRecorder) snapshot() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.at...)
}

func TestCheckoutIdlePollBacksOffOnFakeClock(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 10 * time.Millisecond
		span     = 2 * time.Hour
	)
	synctest.Test(t, func(t *testing.T) {
		rec := &cycleRecorder{started: time.Now()}
		coordinator := scheduleOnlyCoordinator("idle-checkout", ".", interval, quiet, rec.record)
		time.Sleep(span)
		synctest.Wait()
		if err := coordinator.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		at := rec.snapshot()
		// A fixed 15s poll would run 480 cycles in two hours; the backoff
		// schedule runs about 92 (40 at 15s, 20 at 30s, 10 at 60s, 5 at 2m,
		// then the 5m cap).
		if len(at) < 80 || len(at) > 100 {
			t.Fatalf("idle checkout ran %d cycles in %s, want 80..100", len(at), span)
		}
		for i := 1; i < len(at); i++ {
			gap := at[i] - at[i-1]
			if gap > checkoutIdlePollMax+quiet {
				t.Fatalf("poll gap %d = %s exceeds the %s cap", i, gap, checkoutIdlePollMax)
			}
			if at[i] < checkoutIdlePollAfter && gap > interval+quiet {
				t.Fatalf("poll gap %d = %s before the idle threshold, want %s", i, gap, interval)
			}
		}
		if last := at[len(at)-1] - at[len(at)-2]; last != checkoutIdlePollMax {
			t.Fatalf("settled idle gap = %s, want %s", last, checkoutIdlePollMax)
		}
	})
}

func TestCheckoutSelectionWakesBackedOffPollOnce(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 100 * time.Millisecond
	)
	synctest.Test(t, func(t *testing.T) {
		rec := &cycleRecorder{started: time.Now()}
		coordinator := scheduleOnlyCoordinator("selected-checkout", ".", interval, quiet, rec.record)
		time.Sleep(time.Hour)
		synctest.Wait()
		coordinator.mu.Lock()
		backedOff := coordinator.pollBackedOff
		coordinator.mu.Unlock()
		if !backedOff {
			t.Fatal("an hour-idle checkout did not back off its poll")
		}

		before := len(rec.snapshot())
		selectedAt := time.Since(rec.started)
		// A caller polling faster than the quiet window must not starve the
		// wake it asked for: only the first selection signals.
		for range 20 {
			coordinator.PrioritizeSelection()
			time.Sleep(quiet / 10)
		}
		time.Sleep(quiet)
		synctest.Wait()
		at := rec.snapshot()
		if len(at) != before+1 {
			t.Fatalf("selection burst ran %d cycles, want exactly 1", len(at)-before)
		}
		if woke := at[before] - selectedAt; woke != quiet {
			t.Fatalf("selection woke the loop after %s, want the %s quiet window", woke, quiet)
		}

		// The selection restored the base interval for the next poll.
		time.Sleep(interval + quiet)
		synctest.Wait()
		at = rec.snapshot()
		if len(at) != before+2 {
			t.Fatalf("after selection %d cycles ran in one base interval, want 1", len(at)-before-1)
		}
		if gap := at[before+1] - at[before]; gap > interval+quiet {
			t.Fatalf("poll after selection came %s later, want within %s", gap, interval+quiet)
		}
		if err := coordinator.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestDeferDirtyBuildWaitsForQuietTreeWithinCap(t *testing.T) {
	c := &CheckoutCoordinator{}
	start := time.Unix(1_000_000, 0)

	if !c.deferDirtyBuild("edit-1", start) {
		t.Fatal("a freshly moved tree is deferred")
	}
	if !c.deferDirtyBuild("edit-1", start.Add(checkoutDirtyBuildQuiet-time.Second)) {
		t.Fatal("a tree quiet for less than the window stays deferred")
	}
	if c.deferDirtyBuild("edit-1", start.Add(checkoutDirtyBuildQuiet)) {
		t.Fatal("a tree quiet for the whole window is built")
	}

	// Continuous editing restarts the quiet window but not the cap.
	c.clearDirtyDeferral()
	at := start.Add(time.Hour)
	for i := 0; ; i++ {
		now := at.Add(time.Duration(i) * 15 * time.Second)
		deferred := c.deferDirtyBuild(fmt.Sprintf("edit-%d", i), now)
		if now.Sub(at) >= checkoutDirtyBuildMaxDefer {
			if deferred {
				t.Fatalf("still deferred %s into continuous editing, cap is %s", now.Sub(at), checkoutDirtyBuildMaxDefer)
			}
			break
		}
		if !deferred {
			t.Fatalf("edit %d at +%s built before the tree went quiet", i, now.Sub(at))
		}
	}

	// A build or a matching tree ends the run; the next edit starts a new one.
	c.clearDirtyDeferral()
	later := at.Add(time.Hour)
	if !c.deferDirtyBuild("edit-next", later) {
		t.Fatal("a new deferral run did not start after the previous one ended")
	}
}

// An agent that queries between its own edits must not turn every query into a
// rebuild: a selection leaves a deferred working-tree build to the poll.
func TestSelectionDoesNotBypassDeferredWorkingTreeBuild(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 100 * time.Millisecond
	)
	synctest.Test(t, func(t *testing.T) {
		rec := &cycleRecorder{started: time.Now()}
		coordinator := scheduleOnlyCoordinator("deferred-checkout", ".", interval, quiet, rec.record)
		defer coordinator.Close() //nolint:errcheck // the assertions below are what this test checks
		time.Sleep(time.Second)
		synctest.Wait()
		before := len(rec.snapshot())

		coordinator.deferDirtyBuild("pending-edit", time.Now())
		for range 10 {
			coordinator.PrioritizeSelection()
			time.Sleep(quiet)
		}
		synctest.Wait()
		if got := len(rec.snapshot()) - before; got != 0 {
			t.Fatalf("selections of a checkout with deferred changes ran %d cycles, want 0", got)
		}
	})
}

// A fleet that backs off, is selected, defers working-tree changes and is
// closed must leave no goroutine or timer behind: synctest fails the test if
// any goroutine of the bubble is still blocked when the body returns.
func TestCheckoutPollFleetLeavesNoGoroutinesAfterIdleAndClose(t *testing.T) {
	const (
		fleet    = 256
		interval = 15 * time.Second
		quiet    = 10 * time.Millisecond
	)
	synctest.Test(t, func(t *testing.T) {
		coordinators := make([]*CheckoutCoordinator, 0, fleet)
		for i := 0; i < fleet; i++ {
			coordinators = append(coordinators, scheduleOnlyCoordinator(fmt.Sprintf("fleet-%04d", i), ".", interval, quiet, nil))
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		for i, c := range coordinators {
			switch i % 3 {
			case 0:
				c.PrioritizeSelection()
			case 1:
				c.deferDirtyBuild("pending", time.Now())
				c.PrioritizeSelection()
			default:
				c.Signal("source mutation")
			}
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		for _, c := range coordinators {
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if c.Running() {
				t.Fatal("a closed coordinator's loop is still running")
			}
		}
	})
}

func TestCheckoutExplicitSignalRestoresBasePoll(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 10 * time.Millisecond
	)
	synctest.Test(t, func(t *testing.T) {
		rec := &cycleRecorder{started: time.Now()}
		coordinator := scheduleOnlyCoordinator("signalled-checkout", ".", interval, quiet, rec.record)
		time.Sleep(time.Hour)
		synctest.Wait()
		before := len(rec.snapshot())

		defer coordinator.Close() //nolint:errcheck // the assertions below are what this test checks

		coordinator.Signal("source mutation")
		time.Sleep(quiet)
		synctest.Wait()
		// The cycle re-armed the poll at the base interval; it fires one
		// interval after that cycle and runs after its own quiet window.
		time.Sleep(interval + quiet)
		synctest.Wait()
		if got := len(rec.snapshot()) - before; got != 2 {
			t.Fatalf("signal plus one base interval ran %d cycles, want 2", got)
		}
	})
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/savings"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// whatever fn wrote. Tests use this to assert against the dashboard
// output without touching the real terminal.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(&buf, r)
	})

	defer func() {
		os.Stdout = orig
	}()
	fn()
	_ = w.Close()
	wg.Wait()
	return buf.String()
}

func TestSavingsExplainsEstimateBasis(t *testing.T) {
	require.Contains(t, savingsCmd.Long, "gross estimates")
	require.Contains(t, savingsCmd.Long, "not net token savings or measured billing savings")
	require.Contains(t, savingsCmd.Long, "comparison rate, not the agent's detected model")
	for _, calls := range []int64{0, 2} {
		snap := savings.File{Totals: savings.Totals{TokensSaved: 100, TokensReturned: 50, CallsCounted: calls}}
		text := captureStdout(t, func() { emitSavingsDashboard(snap, nil, nil, nil, "fixture.sqlite") })
		require.Contains(t, text, savingsEstimateBasis)
		require.Contains(t, text, savingsCostBasis)
	}
}

func TestSavingsJSONKeepsTotalsAndExplainsEstimates(t *testing.T) {
	snap := savings.File{Totals: savings.Totals{TokensSaved: 100, TokensReturned: 50, CallsCounted: 2}}
	var emitErr error
	text := captureStdout(t, func() { emitErr = emitSavingsJSON(snap, nil, nil, nil, "fixture.sqlite") })
	require.NoError(t, emitErr)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Equal(t, savingsEstimateBasis, out["estimate_basis"])
	require.Equal(t, savingsCostBasis, out["cost_basis"])
	require.Equal(t, float64(100), out["tokens_saved"])
	require.Equal(t, float64(50), out["tokens_returned"])
	require.Equal(t, float64(2), out["calls_counted"])
	costs, ok := out["cost_avoided_usd"].(map[string]any)
	require.True(t, ok)
	for model, cost := range savings.CostAvoidedAll(100) {
		require.Equal(t, cost, costs[model])
	}
}

func TestPickHeadlineCost_RespectsModelFlag(t *testing.T) {
	costs := map[string]float64{
		"claude-opus-4":    0.50,
		"claude-sonnet-4":  0.10,
		"claude-haiku-4.5": 0.03,
	}
	val, name := pickHeadlineCost(costs, "claude-sonnet-4")
	if name != "claude-sonnet-4" || val != 0.10 {
		t.Errorf("pickHeadlineCost(preferred=sonnet) = (%.2f, %q), want (0.10, claude-sonnet-4)", val, name)
	}
}

func TestPickHeadlineCost_DefaultsToMostExpensive(t *testing.T) {
	costs := map[string]float64{
		"claude-opus-4":   0.50,
		"claude-sonnet-4": 0.10,
		"gpt-4o-mini":     0.005,
	}
	val, name := pickHeadlineCost(costs, "")
	if name != "claude-opus-4" || val != 0.50 {
		t.Errorf("pickHeadlineCost(default) = (%.2f, %q), want (0.50, claude-opus-4)", val, name)
	}
}

func TestPickHeadlineCost_AllZeros(t *testing.T) {
	costs := map[string]float64{"a": 0, "b": 0, "c": 0}
	val, name := pickHeadlineCost(costs, "")
	if val != 0 {
		t.Errorf("expected 0 cost for all-zero map, got %.4f", val)
	}
	if name == "" || name == "n/a" {
		t.Errorf("expected a deterministic model name for all-zero map, got %q", name)
	}
}

func TestPickHeadlineCost_Empty(t *testing.T) {
	val, name := pickHeadlineCost(nil, "")
	if val != 0 || name != "n/a" {
		t.Errorf("pickHeadlineCost(nil) = (%.2f, %q), want (0, n/a)", val, name)
	}
}

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0.0000"},
		{0.00125, "$0.0013"},
		{0.99, "$0.9900"},
		{1.00, "$1.00"},
		{167.808, "$167.81"},
	}
	for _, c := range cases {
		got := formatUSD(c.in)
		if got != c.want {
			t.Errorf("formatUSD(%.4f) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanInt(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		got := humanInt(c.in)
		if got != c.want {
			t.Errorf("humanInt(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEmitSavingsDashboard_RendersThreeBuckets is a smoke test for the
// dashboard layout: it captures stdout and asserts each bucket label and a
// representative bar cell show up, so silent regressions in the renderer
// (e.g. a missing bucket, no bar cells written) get caught.
func TestEmitSavingsDashboard_RendersThreeBuckets(t *testing.T) {
	now := time.Now().UTC()
	snap := savings.File{
		FirstSeen:   now.Add(-72 * time.Hour),
		LastUpdated: now,
		Totals:      savings.Totals{TokensSaved: 1_000_000, TokensReturned: 200_000, CallsCounted: 50},
		PerRepo: map[string]*savings.Totals{
			"/repo-a": {TokensSaved: 600_000, TokensReturned: 100_000, CallsCounted: 30},
			"/repo-b": {TokensSaved: 400_000, TokensReturned: 100_000, CallsCounted: 20},
		},
	}
	buckets := []savings.Bucket{
		{Label: "Today", Totals: savings.Totals{TokensSaved: 5_000, TokensReturned: 1_000, CallsCounted: 4},
			PerTool: []savings.ToolTotal{
				{Tool: "get_symbol_source", Totals: savings.Totals{TokensSaved: 4_000, TokensReturned: 800, CallsCounted: 3}},
				{Tool: "smart_context", Totals: savings.Totals{TokensSaved: 1_000, TokensReturned: 200, CallsCounted: 1}},
			}},
		{Label: "Last 7 days", Totals: savings.Totals{TokensSaved: 50_000, TokensReturned: 10_000, CallsCounted: 12}},
		{Label: "All time", Totals: snap.Totals},
	}

	out := captureStdout(t, func() {
		// Force a known cell width and verbose for both code paths.
		oldVerbose := savingsVerbose
		oldCells := savingsBarCells
		savingsVerbose = true
		savingsBarCells = 16
		defer func() {
			savingsVerbose = oldVerbose
			savingsBarCells = oldCells
		}()
		emitSavingsDashboard(snap, buckets, nil, nil, "/tmp/sidecar.sqlite")
	})

	for _, want := range []string{
		"Gortex Token Savings",
		"Cost avoided per model (all time):",
		"Today",
		"Last 7 days",
		"All time",
		"█",                 // at least one filled bar cell
		"░",                 // at least one empty bar cell
		"get_symbol_source", // verbose per-tool table
		"smart_context",
		"Cost avoided per model (all time):",
		"Per-repo totals (all time):",
		"/repo-a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard output missing %q\n----\n%s\n----", want, out)
		}
	}
}

func TestEmitSavingsDashboard_EmptyTotals(t *testing.T) {
	snap := savings.File{}
	buckets := []savings.Bucket{
		{Label: "Today"},
		{Label: "Last 7 days"},
		{Label: "All time"},
	}
	out := captureStdout(t, func() {
		emitSavingsDashboard(snap, buckets, nil, nil, "/tmp/sidecar.sqlite")
	})
	if !strings.Contains(out, "No source-reading tool calls recorded yet") {
		t.Errorf("empty totals should show the no-data hint, got:\n%s", out)
	}
	// Should not print any bucket rows when there's no data.
	if strings.Contains(out, "█") {
		t.Errorf("empty totals should not render bars, got:\n%s", out)
	}
	// An empty ledger has never tracked anything — the dashboard must not
	// claim a "tracking since" moment (the zero FirstSeen stays hidden).
	if strings.Contains(out, "Tracking since") {
		t.Errorf("empty ledger must not print a tracking-since line, got:\n%s", out)
	}
}

// TestDefaultHeadlineModelIsPriced pins the invariant behind the dashboard's
// headline figure. pickHeadlineCost falls back to the highest-value entry
// when defaultHeadlineModel isn't in the pricing table, so an unpriced
// headline model silently re-denominates every headline number against a
// different model instead of failing.
func TestDefaultHeadlineModelIsPriced(t *testing.T) {
	if rate := savings.ModelRate(defaultHeadlineModel); rate <= 0 {
		t.Fatalf("defaultHeadlineModel %q has no pricing row (rate %.4f) — the headline "+
			"cost would silently re-denominate against another model", defaultHeadlineModel, rate)
	}
}

// TestFormatModelCostSeparatesUnpricedFromZero is the fix for a model with
// millions of tokens saved rendering as $0.0000 purely because the pricing
// table had never heard of its id.
func TestFormatModelCostSeparatesUnpricedFromZero(t *testing.T) {
	if got := formatModelCost(20_676_506, "claude-opus-5"); got != "$103.38" {
		t.Errorf("formatModelCost(20.68M, claude-opus-5) = %q, want $103.38", got)
	}
	if got := formatModelCost(5_000_000, "some-unreleased-model"); got != "unpriced" {
		t.Errorf("formatModelCost(5M, unknown model) = %q, want \"unpriced\"", got)
	}
	// A priced model that genuinely saved nothing still reads as a dollar
	// amount — that is a real zero, not a missing rate.
	if got := formatModelCost(0, "claude-opus-5"); got != "$0.0000" {
		t.Errorf("formatModelCost(0, claude-opus-5) = %q, want $0.0000", got)
	}
}

package observability

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexingRegistryExposesPerRepositoryEditFlow(t *testing.T) {
	t.Parallel()
	r := &IndexingRegistry{}
	r.ObservePatch("octopus", 4*time.Second)
	r.ObservePatch("octopus", 2*time.Second)
	r.NoteDeferredPatch("octopus")
	r.ObserveStormDrain("elk-client", 47, 1, 270*time.Second)
	r.ObserveDerivedPasses(1500*time.Millisecond, time.Second)

	var out bytes.Buffer
	r.Write(NewExposition(&out))
	text := out.String()
	for _, want := range []string{
		`gortex_index_patches_total{repo="octopus"} 2`,
		`gortex_index_patch_seconds_total{repo="octopus"} 6`,
		`gortex_index_deferred_patches_total{repo="octopus"} 1`,
		`gortex_index_storm_drain_paths_total{repo="elk-client"} 47`,
		`gortex_index_storm_drain_failed_paths_total{repo="elk-client"} 1`,
		`gortex_index_patch_duration_seconds_bucket{le="5"} 2`,
		`gortex_index_patch_duration_seconds_bucket{le="1"} 0`,
		`gortex_index_storm_drain_duration_seconds_bucket{le="300"} 1`,
		`gortex_index_derived_duration_seconds_count 1`,
		`gortex_index_derived_framework_seconds_total 1`,
	} {
		assert.Contains(t, text, want)
	}
	assert.Contains(t, text, `gortex_index_last_change_timestamp_seconds{repo="octopus"}`)
	assert.Equal(t, 1, strings.Count(text, "# TYPE gortex_index_patches_total counter"), "family metadata is written once")
}

// A label that could break the exposition or explode cardinality never reaches
// it: paths fold to "other", and past the repository ceiling so does the rest.
func TestIndexingRegistryBoundsRepositoryLabels(t *testing.T) {
	t.Parallel()
	r := &IndexingRegistry{}
	r.ObservePatch("a/b", time.Second)
	r.ObservePatch(`bad"quote`, time.Second)
	for i := 0; i < maxIndexingRepos+10; i++ {
		r.NoteDeferredPatch("repo-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26))
	}
	r.mu.Lock()
	n := len(r.repos)
	r.mu.Unlock()
	require.LessOrEqual(t, n, maxIndexingRepos+1)

	var out bytes.Buffer
	r.Write(NewExposition(&out))
	assert.Contains(t, out.String(), `gortex_index_patches_total{repo="other"} 2`)
	assert.NotContains(t, out.String(), `repo="a/b"`)
}

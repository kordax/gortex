package gitpromisor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObjectPresentOnlyInspectsLocalInventory(t *testing.T) {
	fixture := New(t)
	for _, filter := range []string{"blob:none", "tree:0"} {
		t.Run(filter, func(t *testing.T) {
			client := fixture.Clone(t, filter)
			require.True(t, client.ObjectPresent(t, fixture.CommitOID), "clone must contain its commit")
			assert.Equal(t, filter == "blob:none", client.ObjectPresent(t, fixture.RootTreeOID), "root tree availability")
			assert.False(t, client.ObjectPresent(t, fixture.NestedBlobOID), "promised blob must remain missing")
			assert.False(t, client.ObjectPresent(t, strings.Repeat("f", len(fixture.CommitOID))), "unknown object must remain missing")
			require.Zero(t, client.RequestCount(t), "probing local inventory must not contact upload-pack")

			client.FetchAndRequireRequest(t, fixture.NestedBlobOID)
			requests := client.RequestCount(t)
			require.Positive(t, requests, "positive control must contact upload-pack")
			assert.True(t, client.ObjectPresent(t, fixture.NestedBlobOID), "materialized blob must be present")
			assert.Equal(t, requests, client.RequestCount(t), "probing a materialized blob must not fetch again")
		})
	}
}

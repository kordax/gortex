package analysis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type cancellingDeadCodeReader struct {
	*graph.Graph
	cancel context.CancelFunc
}

func (g cancellingDeadCodeReader) DeadCodeCandidates(kinds []graph.NodeKind, edges map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	g.cancel()
	return g.Graph.DeadCodeCandidates(kinds, edges)
}

func TestFindDeadCodeContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	entries, err := FindDeadCodeContext(ctx, nil, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, entries, "cancelled calls must not touch even a nil graph")

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	reader := cancellingDeadCodeReader{Graph: graph.New(), cancel: cancel}
	entries, err = FindDeadCodeContext(ctx, reader, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, entries, "cancellation during candidate acquisition must not report an empty successful analysis")

	entries, err = FindDeadCodeContext(t.Context(), reader.Graph, nil, nil)
	require.NoError(t, err)
	require.Equal(t, FindDeadCode(reader.Graph, nil, nil), entries)
}

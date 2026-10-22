package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
)

func TestIncrementalReplacementPreservesSurvivingIncomingFieldAccess(t *testing.T) {
	for _, scenario := range []string{"survives", "deleted", "kind_changed", "source_replaced", "owner_replaced"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			source := &graph.Node{ID: "repo/use.go::Use", Name: "Use", Kind: graph.KindFunction, FilePath: "repo/use.go"}
			target := &graph.Node{ID: "repo/model.go::Model.Value", Name: "Value", Kind: graph.KindField, FilePath: "repo/model.go"}
			edge := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeAccessesField, FilePath: source.FilePath, Line: 7, Meta: map[string]any{"evidence": "source-owned"}}
			if scenario == "owner_replaced" {
				edge.FilePath = target.FilePath
			}
			store.AddBatch([]*graph.Node{source, target}, []*graph.Edge{edge})
			require.Len(t, store.GetInEdges(target.ID), 1)
			fresh := *target
			fresh.EndLine = 15
			stage := &incrementalBatchStage{graphPath: target.FilePath, priorNodes: []*graph.Node{target}, result: &parser.ExtractionResult{Nodes: []*graph.Node{&fresh}}}
			switch scenario {
			case "deleted":
				stage.result.Nodes = nil
			case "kind_changed":
				fresh.Kind = graph.KindFunction
			}
			stages := []*incrementalBatchStage{stage}
			paths := []string{target.FilePath}
			if scenario == "source_replaced" {
				stages = append(stages, &incrementalBatchStage{graphPath: source.FilePath, priorNodes: []*graph.Node{source}, result: &parser.ExtractionResult{Nodes: []*graph.Node{source}}})
				paths = append(paths, source.FilePath)
			}
			view := incrementalPriorView{inByNode: map[string][]*graph.Edge{target.ID: store.GetInEdges(target.ID)}}
			var nodes []*graph.Node
			for _, item := range stages {
				nodes = append(nodes, item.result.Nodes...)
			}
			require.NoError(t, store.ReplaceFiles(graph.FileBatchReplacement{FilePaths: paths, Nodes: nodes, Edges: incomingRestubsForReplacement(stages, view)}))
			got := store.GetOutEdges(source.ID)
			if scenario == "survives" {
				require.Len(t, got, 1)
				require.Equal(t, edge, got[0])
			} else {
				require.Empty(t, got)
			}
		})
	}
}

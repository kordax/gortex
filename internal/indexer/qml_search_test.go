package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestQMLLiteralSearchTracksIncrementalPublication(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "Main.qml")
	require.NoError(t, os.WriteFile(file, []byte("import QtQuick\nTimer { interval: 200 }\n"), 0o644))
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	g := graph.New()
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(root)
	require.NoError(t, err)
	for range 3 {
		matches := idx.GrepText("interval: 200", 10)
		require.Len(t, matches, 1)
		assert.Equal(t, "Main.qml", matches[0].Path)
		assert.Equal(t, 2, matches[0].Line)
	}
	// Change the size as well as the contents so this fixture does not depend
	// on filesystem timestamp resolution. Search has already been primed.
	require.NoError(t, os.WriteFile(file, []byte("import QtQuick\nTimer { interval: 50 }\n"), 0o644))
	_, err = idx.IncrementalReindexPaths(root, []string{file})
	require.NoError(t, err)
	assert.Empty(t, idx.GrepText("interval: 200", 10))
	require.Len(t, idx.GrepText("interval: 50", 10), 1)
	matches, err := idx.GrepRegexp(`interval:\s+50`, "", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	added := filepath.Join(root, "Added.qml")
	require.NoError(t, os.WriteFile(added, []byte("Timer { interval: 25 }\n"), 0o644))
	_, err = idx.IncrementalReindexPaths(root, []string{added})
	require.NoError(t, err)
	require.Len(t, idx.GrepText("interval: 25", 10), 1)
	require.NoError(t, os.Remove(file))
	_, err = idx.IncrementalReindexPaths(root, []string{file})
	require.NoError(t, err)
	assert.Empty(t, idx.GrepText("interval: 50", 10))
	assert.Empty(t, g.GetFileNodes("Main.qml"))
	require.Len(t, idx.GrepText("interval: 25", 10), 1)
}

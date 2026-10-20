package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestSearchTextQMLReadAgreementAndScope(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	alpha := setupMiniRepoNamed(t, "alpha", "package alpha\nfunc Snapshot() {}\n")
	beta := setupMiniRepoNamed(t, "beta", "package beta\nfunc Snapshot() {}\n")
	const relative = "CacaoPlayer/Main.qml"
	const content = "import QtQuick\nItem {\n    Timer { interval: 200 }\n    Timer { interval: 50 }\n    property string label: \"function notASymbol() {}\"\n}\n"
	for _, root := range []string{alpha, beta} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "CacaoPlayer"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), []byte(content), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(alpha, "private"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(alpha, "private", "Hidden.qml"), []byte(content), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(alpha, ".gortex.yaml"), []byte("index:\n  exclude:\n    - private/**\n"), 0o644))
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: alpha, Name: "alpha"}, {Path: beta, Name: "beta"}}}
	gc.SetConfigPath(configPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: cm, MultiIndexer: mi})

	// Physical reads can succeed even when extraction is unsupported. Assert
	// search agreement separately, through both public tool surfaces.
	read, err := srv.handleReadFile(t.Context(), makeReq("read_file", map[string]any{"path": filepath.Join(alpha, filepath.FromSlash(relative))}))
	require.NoError(t, err)
	require.False(t, read.IsError, viewResultText(t, read))
	require.Contains(t, viewResultText(t, read), "interval: 200")

	for _, facade := range []bool{false, true} {
		for _, regexp := range []bool{false, true} {
			t.Run(fmt.Sprintf("facade=%t/regexp=%t", facade, regexp), func(t *testing.T) {
				args := map[string]any{"repo": "alpha", "regexp": regexp, "path": "CacaoPlayer", "limit": 10}
				req := makeReq("search_text", map[string]any{"query": "interval:", "repo": "alpha", "regexp": regexp, "path": "CacaoPlayer", "limit": 10})
				handler := srv.boundToolHandler(srv.handleSearchText)
				if facade {
					req = makeReq("search", map[string]any{"operation": "text", "query": "interval:", "options": args})
					handler = srv.mcpServer.GetTool("search").Handler
				}
				// Repeating the request also exercises reuse of the search index.
				for range 3 {
					result, callErr := handler(t.Context(), req)
					require.NoError(t, callErr)
					require.False(t, result.IsError, viewResultText(t, result))
					var payload struct {
						Count   int                 `json:"count"`
						Matches []enrichedTextMatch `json:"matches"`
					}
					require.NoError(t, json.Unmarshal([]byte(viewResultText(t, result)), &payload))
					require.Equal(t, 2, payload.Count, "QML reads must not succeed while literal search silently loses the file")
					require.Len(t, payload.Matches, 2)
					for i, match := range payload.Matches {
						assert.Equal(t, "alpha/"+relative, match.Path)
						assert.Equal(t, i+3, match.Line)
						assert.Empty(t, match.SymbolID, "file-only QML support must not invent semantic symbols")
					}
				}
			})
		}
	}
	all, err := srv.handleSearchText(t.Context(), makeReq("search_text", map[string]any{"query": "interval:", "repo": "alpha"}))
	require.NoError(t, err)
	require.False(t, all.IsError, viewResultText(t, all))
	var unfiltered struct {
		Matches []enrichedTextMatch `json:"matches"`
	}
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, all)), &unfiltered))
	require.Len(t, unfiltered.Matches, 2, "excluded QML files and other repositories must stay outside the search corpus")
	for _, match := range unfiltered.Matches {
		assert.Equal(t, "alpha/"+relative, match.Path)
	}

	nodes := g.GetFileNodes("alpha/" + relative)
	require.Len(t, nodes, 1)
	assert.Equal(t, graph.KindFile, nodes[0].Kind)
	assert.Equal(t, "qml", nodes[0].Language)
	assert.Empty(t, g.GetFileNodes("alpha/private/Hidden.qml"))
}

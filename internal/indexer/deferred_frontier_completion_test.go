package indexer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

type deferredFrontierProvider struct {
	name         string
	supplemental bool
	partial      bool
	err          error
	calls        int
}

func (p *deferredFrontierProvider) Name() string        { return p.name }
func (p *deferredFrontierProvider) Languages() []string { return []string{"go"} }
func (p *deferredFrontierProvider) Available() bool     { return true }
func (p *deferredFrontierProvider) Close() error        { return nil }
func (p *deferredFrontierProvider) Supplemental() bool  { return p.supplemental }

func (p *deferredFrontierProvider) Enrich(g graph.Store, root string) (*semantic.EnrichResult, error) {
	return p.EnrichFilesContext(context.Background(), g, "", root, nil)
}

func (p *deferredFrontierProvider) EnrichFile(g graph.Store, root, file string) (*semantic.EnrichResult, error) {
	return p.EnrichFilesContext(context.Background(), g, "", root, []string{file})
}

func (p *deferredFrontierProvider) EnrichFilesContext(_ context.Context, g graph.Store, _, _ string, _ []string) (*semantic.EnrichResult, error) {
	p.calls++
	if !p.supplemental {
		g.AddEdge(&graph.Edge{From: "a.go::A", To: "b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Origin: "frontier-test"})
	}
	return &semantic.EnrichResult{Provider: p.name, Language: "go", Partial: p.partial}, p.err
}

func TestDeferredFrontierRetainsIncompleteSupplementalPass(t *testing.T) {
	for _, mode := range []string{"error", "partial"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "a.go"), "package fixture\nfunc A() {}\n")
			writeFile(t, filepath.Join(root, "b.go"), "package fixture\nfunc B() {}\n")
			gitHome := t.TempDir()
			gitEnv := make([]string, 0, len(os.Environ())+4)
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
					gitEnv = append(gitEnv, entry)
				}
			}
			gitEnv = append(gitEnv, "HOME="+gitHome, "XDG_CONFIG_HOME="+gitHome,
				"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
			for _, args := range [][]string{
				{"init", "--quiet"}, {"add", "a.go", "b.go"},
				{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "fixture"},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
				cmd.Env = gitEnv
				output, err := cmd.CombinedOutput()
				cancel()
				require.NoError(t, err, "isolated git fixture: %s", output)
			}
			store := newFTSStore(t)
			idx := newTestIndexer(store)
			_, err := idx.Index(root)
			require.NoError(t, err)
			manager := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, manager.Close()) })
			primary := &deferredFrontierProvider{name: "primary"}
			supplemental := &deferredFrontierProvider{name: "supplemental", supplemental: true}
			if mode == "error" {
				supplemental.err = errors.New("incomplete supplemental")
			} else {
				supplemental.partial = true
			}
			manager.RegisterProvider(primary)
			manager.RegisterProvider(supplemental)
			idx.semanticMgr = manager
			// Model a resumed exact ledger, rather than the initial full-index
			// pass that the fixture used to populate the persisted graph.
			_, _, initialGeneration := idx.deferredEnrichScope()
			require.True(t, idx.clearPendingEnrich(initialGeneration))
			files := []string{"a.go", "b.go"}
			for _, file := range files {
				idx.setReparsePendingEnrichment(file, true)
			}
			idx.markPendingEnrichFiles(files)
			require.ElementsMatch(t, files, idx.deferredEnrichFrontiers(files)["go"])
			token := store.BeginMutationReceipt()
			idx.runDeferredEnrich()
			receipt := store.EndMutationReceipt(token)
			assert.True(t, idx.pendingEnrich.Load(), "incomplete supplemental provider must retain the retry gate")
			assert.ElementsMatch(t, files, idx.pendingEnrichFrontier(), "incomplete pass must retain durable per-file markers")
			assert.True(t, receipt.Complete)
			assert.Contains(t, receipt.ChangedFiles, "a.go", "primary committed mutations must stay in the receipt")
			require.Len(t, store.GetOutEdges("a.go::A"), 1)
			edges := store.EdgeCount()

			supplemental.err = nil
			supplemental.partial = false
			idx.runDeferredEnrich()
			assert.False(t, idx.pendingEnrich.Load())
			assert.Empty(t, idx.pendingEnrichFrontier())
			assert.Equal(t, edges, store.EdgeCount(), "retry must not duplicate primary committed edges")
			assert.Equal(t, 2, primary.calls)
			assert.Equal(t, 2, supplemental.calls)
		})
	}
}

package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetFileLinesKeepsWholeLeadingLines(t *testing.T) {
	content := []byte("alpha\nbeta\ngamma\ndelta\n")

	out, end, total, ok := budgetFileLines(content, 11) // "alpha\nbeta" is 10 bytes
	require.True(t, ok)
	assert.Equal(t, "alpha\nbeta", string(out))
	assert.Equal(t, 2, end)
	assert.Equal(t, 4, total, "the trailing newline is not a line, matching windowFileLines")

	_, _, _, ok = budgetFileLines([]byte(strings.Repeat("x", 100)), 10)
	assert.False(t, ok, "a first line longer than the budget is left to the byte cap")

	_, end, total, ok = budgetFileLines(content, 1<<20)
	require.True(t, ok)
	assert.Equal(t, total, end, "everything fits")
}

func TestBudgetFileLinesWindowContinuesWithOffset(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		b.WriteString(strings.Repeat("y", 80))
		if i < 499 {
			b.WriteString("\n")
		}
	}
	content := []byte(b.String())
	first, end, total, ok := budgetFileLines(content, 4000)
	require.True(t, ok)
	require.Less(t, end, total)

	rest, applied, start, _, _ := windowFileLines(content, end+1, 0)
	require.True(t, applied)
	assert.Equal(t, end+1, start)
	assert.Equal(t, string(content), string(first)+"\n"+string(rest),
		"the budget window and the continuation window cover the file exactly once")
}

// An unbounded read of a large file returns a leading window inside the
// response budget, says how to continue, and still hands back the whole file
// to a caller that opts out with max_bytes:0.
func TestReadFileLargeFileIsWindowedToBudget(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&b, "line %04d %s\n", i+1, strings.Repeat("z", 60))
	}
	full := b.String()
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.md"), []byte(full), 0o600))
	srv := newReadGuardServer(t, root)

	res := readFileTool(t, srv, map[string]any{"path": "big.md", "format": "json"})
	require.False(t, res.IsError, readText(t, res))
	text := readText(t, res)
	assert.LessOrEqual(t, len(text), defaultMaxBytes, "the whole response stays inside the default budget")
	var got struct {
		Content string `json:"content"`
		Window  struct {
			StartLine  int `json:"start_line"`
			EndLine    int `json:"end_line"`
			TotalLines int `json:"total_lines"`
		} `json:"window"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	assert.Equal(t, 1, got.Window.StartLine)
	assert.Equal(t, 4000, got.Window.TotalLines)
	require.Less(t, got.Window.EndLine, 4000)
	assert.True(t, strings.HasPrefix(full, got.Content), "the window is a prefix of the file")
	assert.Contains(t, text, fmt.Sprintf("offset=%d", got.Window.EndLine+1), "the omission names the continuation")

	whole := readFileTool(t, srv, map[string]any{"path": "big.md", "format": "json", "max_bytes": 0})
	require.False(t, whole.IsError)
	assert.Contains(t, readText(t, whole), "line 4000 ", "max_bytes:0 returns the whole file")

	small := readFileTool(t, srv, map[string]any{"path": "big.md", "format": "json", "offset": 3990})
	require.False(t, small.IsError)
	assert.Contains(t, readText(t, small), "line 4000 ", "an explicit window is never re-cut")
}

// The compressed view of a huge file used to ship whole (360 KB on a
// 13k-line file); it is now cut at whole lines inside half the budget.
func TestGetEditingContextCompressedSourceStaysInBudget(t *testing.T) {
	srv, dir := setupTestServer(t)
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := range 6000 {
		fmt.Fprintf(&b, "var Value%05d = %d\n", i, i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644))
	require.NoError(t, srv.indexer.IndexFile(filepath.Join(dir, "big.go")))

	m := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path": "big.go", "compress_bodies": true, "format": "json",
	}))
	src, _ := m["source_compressed"].(string)
	require.NotEmpty(t, src)
	assert.LessOrEqual(t, len(src), readFileContentBudget(mcplib.CallToolRequest{})/2)
	assert.True(t, strings.HasPrefix(b.String(), src), "the cut keeps a leading run of whole lines")
	assert.Equal(t, true, m["source_compressed_truncated"], "the cut is flagged even when degradation trims omissions")
	assert.Contains(t, m["source_compressed_lines"], "of 6002")

	whole := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path": "big.go", "compress_bodies": true, "format": "json", "max_bytes": 0,
	}))
	all, _ := whole["source_compressed"].(string)
	assert.Contains(t, all, "Value05999", "max_bytes:0 keeps the whole compressed view")
}

func TestClipGrepLineKeepsTheMatchInsideTheCap(t *testing.T) {
	line := strings.Repeat("я", 20_000) + "NEEDLE" + strings.Repeat("ж", 20_000)
	re := regexp.MustCompile("NEEDLE")
	out, cut := clipGrepLine(line, re, ctxGrepLineCap)
	require.True(t, cut)
	assert.Contains(t, out, "NEEDLE")
	assert.True(t, strings.HasPrefix(out, "…") && strings.HasSuffix(out, "…"))
	assert.LessOrEqual(t, len(out), ctxGrepLineCap+2*len("…"))
	assert.True(t, utf8.ValidString(out))

	short, cut := clipGrepLine("plain", re, ctxGrepLineCap)
	assert.False(t, cut)
	assert.Equal(t, "plain", short)
}

func TestReadFileContentBudgetHonorsOptOut(t *testing.T) {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	assert.Positive(t, readFileContentBudget(req), "default reads are budgeted")

	req.Params.Arguments = map[string]any{"max_bytes": 0}
	assert.Zero(t, readFileContentBudget(req), "max_bytes:0 opts out")
}

// A giant symbol used to arrive whole (140 KB observed); it is now cut at
// whole lines inside the budget, flagged, and continues through read_file.
func TestGetSymbolSourceLargeSymbolStaysInBudget(t *testing.T) {
	srv, dir := setupTestServer(t)
	var b strings.Builder
	b.WriteString("package big\n\nfunc Huge() int {\n\tx := 0\n")
	for i := range 6000 {
		fmt.Fprintf(&b, "\tx += %d // step %05d\n", i, i)
	}
	b.WriteString("\treturn x\n}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge.go"), []byte(b.String()), 0o644))
	require.NoError(t, srv.indexer.IndexFile(filepath.Join(dir, "huge.go")))
	var id string
	for _, n := range srv.graph.AllNodes() {
		if n.Name == "Huge" {
			id = n.ID
		}
	}
	require.NotEmpty(t, id)

	m := extractTextResult(t, callTool(t, srv, "get_symbol_source", map[string]any{"id": id, "format": "json"}))
	src, _ := m["source"].(string)
	require.NotEmpty(t, src)
	assert.LessOrEqual(t, len(src), readFileContentBudget(mcplib.CallToolRequest{}))
	assert.Contains(t, src, "func Huge() int {", "the cut keeps the leading lines")
	assert.True(t, strings.HasPrefix(b.String(), src), "with its context lines the cut is a prefix of the file")
	assert.Equal(t, true, m["source_truncated"])
	assert.Contains(t, m["source_lines"], "of 6006")

	whole := extractTextResult(t, callTool(t, srv, "get_symbol_source", map[string]any{"id": id, "format": "json", "max_bytes": 0}))
	all, _ := whole["source"].(string)
	assert.Contains(t, all, "step 05999", "max_bytes:0 keeps the whole symbol")
	assert.Nil(t, whole["source_truncated"])
}

// batch_symbols shares the budget across its symbols: a giant body is cut,
// a small neighbour arrives whole.
func TestBatchSymbolsSharesTheBudget(t *testing.T) {
	srv, dir := setupTestServer(t)
	var b strings.Builder
	b.WriteString("package big\n\nfunc Small() int { return 1 }\n\nfunc Huge() int {\n\tx := 0\n")
	for i := range 6000 {
		fmt.Fprintf(&b, "\tx += %d // step %05d\n", i, i)
	}
	b.WriteString("\treturn x\n}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge.go"), []byte(b.String()), 0o644))
	require.NoError(t, srv.indexer.IndexFile(filepath.Join(dir, "huge.go")))
	ids := map[string]string{}
	for _, n := range srv.graph.AllNodes() {
		if n.Name == "Huge" || n.Name == "Small" {
			ids[n.Name] = n.ID
		}
	}
	require.Len(t, ids, 2)

	m := extractTextResult(t, callTool(t, srv, "batch_symbols", map[string]any{
		"ids": ids["Small"] + "," + ids["Huge"], "include_source": true, "format": "json",
	}))
	syms, _ := m["symbols"].([]any)
	require.Len(t, syms, 2)
	byName := map[string]map[string]any{}
	for _, s := range syms {
		e := s.(map[string]any)
		byName[e["name"].(string)] = e
	}
	assert.Contains(t, byName["Small"]["source"], "return 1")
	assert.Nil(t, byName["Small"]["source_truncated"])
	huge, _ := byName["Huge"]["source"].(string)
	assert.LessOrEqual(t, len(huge), readFileContentBudget(mcplib.CallToolRequest{})/2)
	assert.Equal(t, true, byName["Huge"]["source_truncated"])
}

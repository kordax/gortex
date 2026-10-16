package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"

	"github.com/zzet/gortex/internal/graph"
)

func TestSearchSymbolMetadataRelaxedFiltersAcrossFormats(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, format := range []string{"json", "gcx", "toon", "compact"} {
		t.Run(format, func(t *testing.T) {
			req := searchMetadataRequest(format, map[string]any{"query": "helper", "kind": "interface"})
			result, err := srv.handleSearchSymbols(context.Background(), req)
			require.NoError(t, err)
			require.False(t, result.IsError)
			text, ok := singleTextContent(result)
			require.True(t, ok)
			assert.Contains(t, text, "helper")
			assert.Contains(t, text, "filters_relaxed")
			if format == "gcx" {
				decoder := wire.NewDecoder(strings.NewReader(text))
				header, decodeErr := decoder.Header()
				require.NoError(t, decodeErr)
				assert.Equal(t, "true", header.Meta["filters_relaxed"])
				rows, decodeErr := decoder.All()
				require.NoError(t, decodeErr)
				require.Len(t, rows, 1)
				assert.Equal(t, "function", rows[0]["kind"])
			}
		})
	}
}

func TestSearchSymbolMetadataNativeDocuments(t *testing.T) {
	srv := &Server{}
	page := []*graph.Node{newTestNode("main.go::Helper", "Helper", graph.KindFunction, "main.go", 7)}
	payload := map[string]any{
		"results":         []map[string]any{page[0].Brief()},
		"total":           3,
		"truncated":       true,
		"next_cursor":     encodeCursor(1),
		"query_class":     "symbol",
		"filters_relaxed": true,
		"decomposed":      true,
		"fetch_escalated": true,
		"scope_note":      "Some matches are outside the selected repository.",
		"query_advice":    map[string]any{"reason": "Use one concept per query.", "split_into": []string{"decode", "body"}},
		"content_matches": map[string]any{"results": []map[string]any{{"path": "config.go", "line": 9, "text": "literal with spaces\nand a newline"}}},
		"assist":          map[string]any{"engaged": true},
	}
	for _, format := range []string{"json", "gcx", "toon", "compact"} {
		t.Run(format, func(t *testing.T) {
			result, err := srv.respondSearchSymbols(context.Background(), searchMetadataRequest(format, nil), page, 0, 3, payload, ResolvedScope{})
			require.NoError(t, err)
			require.False(t, result.IsError)
			text, ok := singleTextContent(result)
			require.True(t, ok)
			for _, field := range []string{"next_cursor", "query_class", "filters_relaxed", "decomposed", "fetch_escalated", "scope_note", "query_advice", "content_matches", "assist"} {
				assert.Contains(t, text, field)
			}
			switch format {
			case "json":
				var decoded map[string]any
				require.NoError(t, json.Unmarshal([]byte(text), &decoded))
				assert.Equal(t, true, decoded["filters_relaxed"])
			case "gcx":
				decoder := wire.NewDecoder(strings.NewReader(text))
				header, decodeErr := decoder.Header()
				require.NoError(t, decodeErr)
				assert.Equal(t, "search_symbols", header.Tool)
				assert.Equal(t, "true", header.Meta["decomposed"])
				assert.Equal(t, encodeCursor(1), header.Meta["next_cursor"])
				rows, decodeErr := decoder.All()
				require.NoError(t, decodeErr)
				require.Len(t, rows, 1)
				assert.Equal(t, "main.go::Helper", rows[0]["id"])
				assert.Equal(t, "func Helper()", rows[0]["sig"])
			case "toon":
				var decoded map[string]any
				require.NoError(t, toon.Unmarshal([]byte(text), &decoded))
				assert.Equal(t, true, decoded["filters_relaxed"])
				assert.Equal(t, encodeCursor(1), decoded["next_cursor"])
			case "compact":
				assert.True(t, strings.HasPrefix(text, "function Helper main.go:7\n"))
				_, metadata, found := strings.Cut(text, "search_metadata:\n")
				require.True(t, found)
				var decoded map[string]any
				require.NoError(t, toon.Unmarshal([]byte("search_metadata:\n"+metadata), &decoded))
			}
		})
	}
}

func TestSearchSymbolMetadataGCXLastPage(t *testing.T) {
	page := []*graph.Node{newTestNode("x.go::Last", "Last", graph.KindFunction, "x.go", 1)}
	payload, metadata, err := encodeBudgetedSearchSymbols(searchMetadataRequest("gcx", nil), page, 4, 5, map[string]any{"query_class": "symbol"})
	require.NoError(t, err)
	decoder := wire.NewDecoder(strings.NewReader(string(payload)))
	header, err := decoder.Header()
	require.NoError(t, err)
	assert.Equal(t, "false", header.Meta["truncated"])
	assert.NotContains(t, header.Meta, "next_cursor")
	assert.Equal(t, false, metadata["truncated"])
	assert.NotContains(t, metadata, "next_cursor")
}

func TestSearchSymbolMetadataGCXBudgetCursor(t *testing.T) {
	page := make([]*graph.Node, 4)
	for i := range page {
		page[i] = newTestNode(fmt.Sprintf("x.go::Helper%d", i), fmt.Sprintf("Helper%d", i), graph.KindFunction, "x.go", i+1)
		page[i].Meta["signature"] = "func " + page[i].Name + "(" + strings.Repeat("a", 1400) + ")"
	}
	for _, budget := range []int{2300, 0} {
		t.Run(fmt.Sprintf("max_bytes_%d", budget), func(t *testing.T) {
			req := searchMetadataRequest("gcx", map[string]any{"max_bytes": budget})
			payload, metadata, err := encodeBudgetedSearchSymbols(req, page, 3, 20, map[string]any{"query_class": "symbol"})
			require.NoError(t, err)
			decoder := wire.NewDecoder(strings.NewReader(string(payload)))
			header, err := decoder.Header()
			require.NoError(t, err)
			rows, err := decoder.All()
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			assert.Equal(t, 3+len(rows), decodeCursor(header.Meta["next_cursor"]), "the next request must start after the emitted prefix, without skipping budget-trimmed hits")
			assert.Equal(t, header.Meta["next_cursor"], metadata["next_cursor"])
			if budget > 0 {
				assert.Less(t, len(rows), len(page))
				assert.LessOrEqual(t, len(payload), budget)
				assert.Equal(t, "true", header.Meta["truncated_by_budget"])
			} else {
				assert.Len(t, rows, len(page))
				assert.NotContains(t, header.Meta, "truncated_by_budget")
			}
		})
	}
}

func TestSearchSymbolMetadataGCXBudgetRetainsOneRow(t *testing.T) {
	page := []*graph.Node{newTestNode("x.go::Helper", "Helper", graph.KindFunction, "x.go", 1)}
	payload, metadata, err := encodeBudgetedSearchSymbols(searchMetadataRequest("gcx", map[string]any{"max_bytes": 1}), page, 0, 1, nil)
	require.NoError(t, err)
	decoder := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = decoder.Header()
	require.NoError(t, err)
	rows, err := decoder.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, true, metadata["truncated_by_budget"])
}

func TestSearchSymbolMetadataGCXTokenBudgetAnnotation(t *testing.T) {
	page := make([]*graph.Node, 4)
	for i := range page {
		page[i] = newTestNode(fmt.Sprintf("x.go::Helper%d", i), fmt.Sprintf("Helper%d", i), graph.KindFunction, "x.go", i+1)
		page[i].Meta["signature"] = "func " + page[i].Name + "(" + strings.Repeat("a", 1400) + ")"
	}
	req := searchMetadataRequest("gcx", map[string]any{"max_tokens": 600})
	payload, metadata, err := encodeBudgetedSearchSymbols(req, page, 0, len(page), nil)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(payload), effectiveBudget(req))
	assert.Contains(t, string(payload), "# truncated_by_budget=true")
	assert.Contains(t, string(payload), "# max_tokens=600 truncated_by_tokens=true")
	assert.Equal(t, true, metadata["truncated_by_budget"])
	decoder := wire.NewDecoder(strings.NewReader(string(payload)))
	header, err := decoder.Header()
	require.NoError(t, err)
	rows, err := decoder.All()
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Less(t, len(rows), len(page))
	assert.Equal(t, len(rows), decodeCursor(header.Meta["next_cursor"]))

	req = searchMetadataRequest("gcx", map[string]any{"max_tokens": 10000})
	payload, _, err = encodeBudgetedSearchSymbols(req, page, 0, len(page), nil)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "truncated_by_budget")
	assert.NotContains(t, string(payload), "truncated_by_tokens")
}

func TestSearchSymbolMetadataGCXVisiblePageAttribution(t *testing.T) {
	srv, _ := setupTestServer(t)
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("BudgetedHelper%d", i)
		node := newTestNode("main.go::"+name, name, graph.KindFunction, "main.go", i+20)
		node.Meta["signature"] = "func " + name + "(" + strings.Repeat("a", 1400) + ")"
		srv.graph.AddNode(node)
	}
	ctx := WithSessionID(context.Background(), "search-visible-page")
	query := "BudgetedHelper"
	fullRequest := searchMetadataRequest("gcx", map[string]any{"query": query, "limit": 4, "max_bytes": 0, "assist": "off", "expand": "off"})
	full, err := srv.handleSearchSymbols(ctx, fullRequest)
	require.NoError(t, err)
	require.False(t, full.IsError)
	fullText, ok := singleTextContent(full)
	require.True(t, ok)
	fullDecoder := wire.NewDecoder(strings.NewReader(fullText))
	_, err = fullDecoder.Header()
	require.NoError(t, err)
	fullRows, err := fullDecoder.All()
	require.NoError(t, err)
	require.Len(t, fullRows, 4)

	const reservation = uint64(7)
	capturing := withLocalizationPermittedEvidenceCapture(ctx, reservation)
	request := searchMetadataRequest("gcx", map[string]any{"query": query, "limit": 4, "max_bytes": 2300, "assist": "off", "expand": "off"})
	result, err := srv.handleSearchSymbols(capturing, request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text, ok := singleTextContent(result)
	require.True(t, ok)
	decoder := wire.NewDecoder(strings.NewReader(text))
	header, err := decoder.Header()
	require.NoError(t, err)
	rows, err := decoder.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, decodeCursor(header.Meta["next_cursor"]))

	evidence, recorded := localizationCapturedEvidence(capturing, reservation)
	require.True(t, recorded)
	require.Len(t, evidence, 1, "localization must not claim symbols trimmed from the emitted page")
	assert.Equal(t, rows[0]["id"], evidence[0].ID)
	session := srv.sessionFor(ctx)
	assert.Equal(t, query, session.attributedQuery(rows[0]["id"]))
	for _, row := range fullRows {
		if row["id"] != rows[0]["id"] {
			assert.Empty(t, session.attributedQuery(row["id"]), "unseen rows must not receive search feedback attribution")
		}
	}

	const emptyReservation = uint64(8)
	emptyCapture := withLocalizationPermittedEvidenceCapture(ctx, emptyReservation)
	emptyRequest := searchMetadataRequest("gcx", map[string]any{"query": query, "limit": 4, "cursor": encodeCursor(100), "assist": "off", "expand": "off"})
	empty, err := srv.handleSearchSymbols(emptyCapture, emptyRequest)
	require.NoError(t, err)
	require.False(t, empty.IsError)
	emptyEvidence, recorded := localizationCapturedEvidence(emptyCapture, emptyReservation)
	require.True(t, recorded)
	assert.Empty(t, emptyEvidence)
	assert.Empty(t, session.attributedQuery(rows[0]["id"]), "an empty emitted page must clear prior attribution")
}

func TestSearchSymbolMetadataJSONVisiblePage(t *testing.T) {
	srv := &Server{}
	page := make([]*graph.Node, 4)
	results := make([]map[string]any, len(page))
	for i := range page {
		name := fmt.Sprintf("Helper%d", i) + strings.Repeat("a", 500)
		page[i] = newTestNode("x.go::"+name, name, graph.KindFunction, "x.go", i+1)
		results[i] = page[i].Brief()
	}
	payload := map[string]any{"results": results, "total": len(page), "truncated": false}
	rendered, err := srv.renderSearchSymbols(context.Background(), searchMetadataRequest("json", map[string]any{"max_bytes": 1400}), page, 0, len(page), payload, ResolvedScope{})
	require.NoError(t, err)
	text, ok := singleTextContent(rendered.result)
	require.True(t, ok)
	var decoded struct {
		Results []json.RawMessage `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &decoded))
	assert.Less(t, len(decoded.Results), len(page))
	assert.Len(t, rendered.nodes, len(decoded.Results))
	for i, node := range rendered.nodes {
		assert.Same(t, page[i], node)
	}
}

func TestSearchSymbolMetadataReturnedPageConservative(t *testing.T) {
	page := []*graph.Node{
		newTestNode("x.go::First", "First", graph.KindFunction, "x.go", 1),
		newTestNode("x.go::Second", "Second", graph.KindFunction, "x.go", 2),
	}
	for _, tc := range []struct {
		name   string
		result *mcp.CallToolResult
		kept   int
	}{
		{name: "nil"},
		{name: "error", result: mcp.NewToolResultError("failed")},
		{name: "empty", result: &mcp.CallToolResult{}},
		{name: "malformed", result: mcp.NewToolResultText("[invalid")},
		{name: "sparse_json", result: mcp.NewToolResultText(`{"results":[{"name":"First"}]}`), kept: 1},
		{name: "toon_fallback", result: mcp.NewToolResultText("results[1]{name}:\n  First\n"), kept: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visible := searchSymbolsResultPage(tc.result, page)
			assert.Len(t, visible, tc.kept)
			for i, node := range visible {
				assert.Same(t, page[i], node)
			}
		})
	}
}

func searchMetadataRequest(format string, arguments map[string]any) mcp.CallToolRequest {
	args := make(map[string]any, len(arguments)+2)
	for key, value := range arguments {
		args[key] = value
	}
	if format == "compact" {
		args["compact"] = true
		args["format"] = "json"
	} else {
		args["format"] = format
	}
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "search_symbols", Arguments: args}}
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	toon "github.com/toon-format/toon-go"

	"github.com/zzet/gortex/internal/graph"
)

type searchSymbolsRenderedPage struct {
	result *mcp.CallToolResult
	nodes  []*graph.Node
}

// Search metadata is assembled once before choosing a wire format. The result
// rows retain each format's native schema; recovery and pagination information
// must remain visible alongside those rows.
func (s *Server) respondSearchSymbols(ctx context.Context, req mcp.CallToolRequest, page []*graph.Node, offset, total int, payload map[string]any, scope ResolvedScope) (*mcp.CallToolResult, error) {
	rendered, err := s.renderSearchSymbols(ctx, req, page, offset, total, payload, scope)
	return rendered.result, err
}

func (s *Server) renderSearchSymbols(ctx context.Context, req mcp.CallToolRequest, page []*graph.Node, offset, total int, payload map[string]any, scope ResolvedScope) (searchSymbolsRenderedPage, error) {
	if !isCompact(req) && !s.isGCX(ctx, req) && !s.isTOON(ctx, req) {
		result, err := s.respondScopedJSONOrTOON(ctx, req, payload, scope)
		return searchSymbolsRenderedPage{result: result, nodes: searchSymbolsResultPage(result, page)}, err
	}
	metadata := maps.Clone(payload)
	delete(metadata, "results")
	visible := page
	var text string
	if isCompact(req) {
		encoded, err := toon.Marshal(metadata)
		if err != nil {
			return searchSymbolsRenderedPage{}, err
		}
		text = compactNodes(page) + "search_metadata:\n  " + strings.ReplaceAll(strings.TrimRight(string(encoded), "\n"), "\n", "\n  ") + "\n"
	} else if s.isGCX(ctx, req) {
		encoded, err := budgetSearchSymbolsPage(req, page, offset, total, metadata)
		if err != nil {
			return searchSymbolsRenderedPage{result: mcp.NewToolResultError(fmt.Sprintf("wire encode failed: %v", err))}, nil
		}
		text, metadata = string(encoded.payload), encoded.metadata
		visible = page[:encoded.kept]
	} else {
		toonPayload := maps.Clone(payload)
		toonPayload["results"] = nodesToTOONRows(page)
		encoded, err := toon.Marshal(toonPayload)
		if err != nil {
			result, fallbackErr := s.respondScopedJSONOrTOON(ctx, req, payload, scope)
			return searchSymbolsRenderedPage{result: result, nodes: searchSymbolsResultPage(result, page)}, fallbackErr
		}
		text = string(encoded)
	}
	result := mergeResultMeta(mcp.NewToolResultText(text), metadata)
	return searchSymbolsRenderedPage{result: decorateResultWithScope(result, scope), nodes: visible}, nil
}

// The generic response budget also trims result tails. Count its actual rows
// after encoding, including sparse fieldsets where IDs may be omitted. Failed
// or unrecognized responses provide no evidence of a returned search page.
func searchSymbolsResultPage(result *mcp.CallToolResult, page []*graph.Node) []*graph.Node {
	if result == nil || result.IsError {
		return nil
	}
	text, ok := singleTextContent(result)
	if !ok {
		return nil
	}
	var payload struct {
		Results []any `json:"results" toon:"results"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		if err := toon.Unmarshal([]byte(text), &payload); err != nil {
			return nil
		}
	}
	return page[:min(len(page), len(payload.Results))]
}

// Header tokens carry scalars. Structured values and prose are valid GCX
// comments, written before the result rows so they cannot become an invalid
// JSON suffix or a final section that defeats the row-tail budget.
func searchSymbolsGCXMetadata(metadata map[string]any) ([]string, []string, error) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if key != "results" && key != "total" && key != "truncated" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var header, comments []string
	for _, key := range keys {
		value := metadata[key]
		if scalar, ok := gcxHeaderValue(value); ok && gcxHeaderSafe(key) && gcxHeaderSafe(scalar) {
			header = append(header, key, scalar)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, nil, err
		}
		comments = append(comments, key+"="+string(encoded))
	}
	return header, comments, nil
}

func searchSymbolsPageMetadata(metadata map[string]any, offset, kept, total int, trimmed bool) map[string]any {
	result := maps.Clone(metadata)
	if result == nil {
		result = make(map[string]any)
	}
	result["total"] = total
	result["truncated"] = offset+kept < total
	delete(result, "next_cursor")
	if kept > 0 && offset+kept < total {
		result["next_cursor"] = encodeCursor(offset + kept)
	}
	if rows, ok := result["rerank"].([]map[string]any); ok && len(rows) > kept {
		result["rerank"] = rows[:kept]
	}
	if trimmed {
		result["truncated_by_budget"] = true
	}
	return result
}

type searchSymbolsEncodedPage struct {
	payload  []byte
	metadata map[string]any
	kept     int
}

func encodeBudgetedSearchSymbols(req mcp.CallToolRequest, page []*graph.Node, offset, total int, metadata map[string]any) ([]byte, map[string]any, error) {
	encoded, err := budgetSearchSymbolsPage(req, page, offset, total, metadata)
	return encoded.payload, encoded.metadata, err
}

// Budget the native row prefix before publishing its cursor. Trimming an
// already-paginated payload would advance past rows the caller never received.
// As with the common GCX budget, one row remains when no row fits the cap.
func budgetSearchSymbolsPage(req mcp.CallToolRequest, page []*graph.Node, offset, total int, metadata map[string]any) (searchSymbolsEncodedPage, error) {
	budget := effectiveBudget(req)
	reserve := tokenBudgetDecorationReserve(req)
	decorate := reserve == 0 || reserve < budget
	if reserve > 0 && decorate {
		budget -= reserve
	}
	encode := func(kept int, trimmed bool) ([]byte, map[string]any, error) {
		meta := searchSymbolsPageMetadata(metadata, offset, kept, total, trimmed)
		encoded, err := encodeSearchSymbolsWithMetadata(page[:kept], total, kept, meta)
		if err == nil && trimmed {
			// Keep the common budget marker so the token annotation decorator
			// recognizes this trim. Count the marker when choosing the prefix.
			encoded = fmt.Appendf(encoded, "# truncated_by_budget=true original_rows=%d kept_rows=%d\n", len(page), kept)
		}
		return encoded, meta, err
	}
	kept := len(page)
	encoded, finalMetadata, err := encode(kept, false)
	if err != nil || budget <= 0 || len(encoded) <= budget {
		return searchSymbolsEncodedPage{payload: encoded, metadata: finalMetadata, kept: kept}, err
	}
	if len(page) == 0 {
		encoded, finalMetadata, err = encode(0, true)
	} else {
		low, high := 1, len(page)
		best := 1
		for low <= high {
			mid := low + (high-low)/2
			candidate, _, encodeErr := encode(mid, true)
			if encodeErr != nil {
				return searchSymbolsEncodedPage{}, encodeErr
			}
			if len(candidate) <= budget {
				best, low = mid, mid+1
			} else {
				high = mid - 1
			}
		}
		kept = best
		encoded, finalMetadata, err = encode(kept, true)
	}
	if err == nil && decorate {
		encoded = decorateTokenBudgetGCX(encoded, req)
	}
	return searchSymbolsEncodedPage{payload: encoded, metadata: finalMetadata, kept: kept}, err
}

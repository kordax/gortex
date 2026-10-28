package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/tokens"
)

// registerPostFilterTools wires the response-handle post-filter tools.
// Gortex captures every large tool response into a per-session ring;
// these tools re-cut a captured response (grep, slice, peek) without
// re-issuing the original — potentially expensive — query.
func (s *Server) registerPostFilterTools() {
	s.addTool(
		mcp.NewTool("ctx_stats",
			mcp.WithDescription("Lists the session's buffered tool responses. Gortex automatically captures every large tool result so it can be re-cut later — call this with no arguments to see all handles (response_id, tool, line / byte / token counts), or pass response_id for one entry. The companion tools ctx_peek, ctx_slice, ctx_grep and head_results re-cut a captured response without re-running the query."),
			mcp.WithString("response_id", mcp.Description("Handle of a buffered response. Omit to list every buffered response in the session.")),
		),
		s.handleCtxStats,
	)

	s.addTool(
		mcp.NewTool("ctx_peek",
			mcp.WithDescription("Previews a buffered tool response: its first and last N lines plus line / byte counts, with the middle elided. Use it to orient in a large prior result before slicing or grepping it."),
			mcp.WithString("response_id", mcp.Description("Handle of a buffered response (see ctx_stats). Omit for the most recent capture.")),
			mcp.WithNumber("lines", mcp.Description("Lines to show from each end (default 20).")),
		),
		s.handleCtxPeek,
	)

	s.addTool(
		mcp.NewTool("head_results",
			mcp.WithDescription("Returns the first N lines of a buffered tool response — the quick look at the top of a large prior result, without re-running the query."),
			mcp.WithString("response_id", mcp.Description("Handle of a buffered response (see ctx_stats). Omit for the most recent capture.")),
			mcp.WithNumber("lines", mcp.Description("Number of leading lines to return (default 40).")),
		),
		s.handleHeadResults,
	)

	s.addTool(
		mcp.NewTool("ctx_slice",
			mcp.WithDescription("Returns an explicit line range [from, to] of a buffered tool response — re-read part of a large prior result without re-querying."),
			mcp.WithString("response_id", mcp.Description("Handle of a buffered response (see ctx_stats). Omit for the most recent capture.")),
			mcp.WithNumber("from", mcp.Description("1-based start line, inclusive (default 1).")),
			mcp.WithNumber("to", mcp.Description("1-based end line, inclusive (default: last line).")),
		),
		s.handleCtxSlice,
	)

	grepDesc := "Greps a buffered tool response: returns the matching lines as a structured matches[] list and a grep-style block (N: prefixes a match line, N- a context line, -- separates non-adjacent groups). Re-cut a large prior result down to what matters instead of re-issuing the query. Supports RE2 regex (or a literal string), case-insensitive matching, and -A/-B/-C context."
	grepParams := []mcp.ToolOption{
		mcp.WithDescription(grepDesc),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Pattern to search for — an RE2 regular expression unless `literal` is set.")),
		mcp.WithString("response_id", mcp.Description("Handle of a buffered response (see ctx_stats). Omit for the most recent capture.")),
		mcp.WithBoolean("literal", mcp.Description("Treat pattern as a literal string rather than a regex.")),
		mcp.WithBoolean("ignore_case", mcp.Description("Case-insensitive match.")),
		mcp.WithNumber("context", mcp.Description("Context lines to show on both sides of each match (default 0).")),
		mcp.WithNumber("before", mcp.Description("Context lines before each match (overrides `context`).")),
		mcp.WithNumber("after", mcp.Description("Context lines after each match (overrides `context`).")),
		mcp.WithNumber("max_matches", mcp.Description("Cap on the number of matches returned (default 100).")),
	}
	s.addTool(mcp.NewTool("ctx_grep", grepParams...), s.handleCtxGrep)
	// grep_results is the repomix-compatible name for the same tool.
	s.addTool(mcp.NewTool("grep_results", grepParams...), s.handleCtxGrep)
}

// resolveBufferedResponse looks up the response_id argument (defaulting
// to the most recent capture) and returns either the entry or a ready
// error result.
func (s *Server) resolveBufferedResponse(ctx context.Context, req mcp.CallToolRequest) (bufferedResponse, *mcp.CallToolResult) {
	id := req.GetString("response_id", "")
	e, ok := s.responseBufferFor(ctx).get(id)
	if !ok {
		if id == "" {
			return bufferedResponse{}, mcp.NewToolResultError("no tool responses are buffered in this session yet")
		}
		return bufferedResponse{}, mcp.NewToolResultError("no buffered response with id " + id)
	}
	return e, nil
}

// responseStatsMap summarizes one buffered response for ctx_stats.
func responseStatsMap(e bufferedResponse) map[string]any {
	lines := 0
	if e.Text != "" {
		lines = strings.Count(e.Text, "\n") + 1
	}
	return map[string]any{
		"response_id":   e.ID,
		"tool":          e.Tool,
		"lines":         lines,
		"bytes":         len(e.Text),
		"approx_tokens": int(tokens.CachedCountInt64(e.Text)),
		"captured_at":   e.CapturedAt.UTC().Format(time.RFC3339),
		"age_seconds":   int(time.Since(e.CapturedAt).Seconds()),
	}
}

func (s *Server) handleCtxStats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	buf := s.responseBufferFor(ctx)
	if id := req.GetString("response_id", ""); id != "" {
		e, ok := buf.get(id)
		if !ok {
			return mcp.NewToolResultError("no buffered response with id " + id), nil
		}
		return mcp.NewToolResultJSON(responseStatsMap(e))
	}
	list := buf.list()
	entries := make([]map[string]any, 0, len(list))
	for _, e := range list {
		entries = append(entries, responseStatsMap(e))
	}
	return mcp.NewToolResultJSON(map[string]any{
		"buffered": entries,
		"count":    len(entries),
	})
}

func (s *Server) handleHeadResults(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e, errRes := s.resolveBufferedResponse(ctx, req)
	if errRes != nil {
		return errRes, nil
	}
	n := req.GetInt("lines", 40)
	if n < 1 {
		n = 40
	}
	lines := strings.Split(e.Text, "\n")
	total := len(lines)
	if n > total {
		n = total
	}
	return mcp.NewToolResultJSON(map[string]any{
		"response_id": e.ID,
		"tool":        e.Tool,
		"from_line":   1,
		"to_line":     n,
		"total_lines": total,
		"truncated":   n < total,
		"content":     strings.Join(lines[:n], "\n"),
	})
}

func (s *Server) handleCtxSlice(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e, errRes := s.resolveBufferedResponse(ctx, req)
	if errRes != nil {
		return errRes, nil
	}
	lines := strings.Split(e.Text, "\n")
	total := len(lines)
	from := req.GetInt("from", 1)
	to := req.GetInt("to", total)
	if from < 1 {
		from = 1
	}
	if to > total {
		to = total
	}
	if from > total || to < from {
		return mcp.NewToolResultError(fmt.Sprintf("invalid range: from=%d to=%d (response has %d lines)", from, to, total)), nil
	}
	return mcp.NewToolResultJSON(map[string]any{
		"response_id": e.ID,
		"tool":        e.Tool,
		"from_line":   from,
		"to_line":     to,
		"total_lines": total,
		"content":     strings.Join(lines[from-1:to], "\n"),
	})
}

func (s *Server) handleCtxPeek(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e, errRes := s.resolveBufferedResponse(ctx, req)
	if errRes != nil {
		return errRes, nil
	}
	n := req.GetInt("lines", 20)
	if n < 1 {
		n = 20
	}
	lines := strings.Split(e.Text, "\n")
	total := len(lines)
	headN := n
	if headN > total {
		headN = total
	}
	tailFrom := total - n
	if tailFrom < headN {
		tailFrom = headN // head and tail must not overlap
	}
	out := map[string]any{
		"response_id":  e.ID,
		"tool":         e.Tool,
		"total_lines":  total,
		"bytes":        len(e.Text),
		"head":         strings.Join(lines[:headN], "\n"),
		"elided_lines": tailFrom - headN,
	}
	if tailFrom < total {
		out["tail"] = strings.Join(lines[tailFrom:], "\n")
		out["tail_from_line"] = tailFrom + 1
	}
	return mcp.NewToolResultJSON(out)
}

func (s *Server) handleCtxGrep(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e, errRes := s.resolveBufferedResponse(ctx, req)
	if errRes != nil {
		return errRes, nil
	}
	pattern, err := req.RequireString("pattern")
	if err != nil {
		return mcp.NewToolResultError("pattern is required"), nil
	}
	expr := pattern
	if req.GetBool("literal", false) {
		expr = regexp.QuoteMeta(expr)
	}
	if req.GetBool("ignore_case", false) {
		expr = "(?i)" + expr
	}
	re, cerr := regexp.Compile(expr)
	if cerr != nil {
		return mcp.NewToolResultError("invalid pattern: " + cerr.Error()), nil
	}

	ctxLines := req.GetInt("context", 0)
	before := req.GetInt("before", ctxLines)
	after := req.GetInt("after", ctxLines)
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	maxMatches := req.GetInt("max_matches", 100)
	if maxMatches < 1 {
		maxMatches = 100
	}

	lines := strings.Split(e.Text, "\n")
	var matchRows []int
	for i, ln := range lines {
		if re.MatchString(ln) {
			matchRows = append(matchRows, i)
		}
	}
	total := len(matchRows)
	truncated := false
	if len(matchRows) > maxMatches {
		matchRows = matchRows[:maxMatches]
		truncated = true
	}

	// A buffered response is often one JSON line of hundreds of KB; echoing
	// a matched line whole (twice, in matches and block) shipped up to 487 KB.
	// Matched lines keep a window around their first match, context lines
	// their head.
	clipped := false
	view := make([]string, len(lines))
	copy(view, lines)
	shown := make(map[int]bool, len(matchRows))
	for _, i := range matchRows {
		shown[i] = true
		var cut bool
		view[i], cut = clipGrepLine(lines[i], re, ctxGrepLineCap)
		clipped = clipped || cut
	}
	for _, i := range matchRows {
		for j := max(0, i-before); j <= min(len(lines)-1, i+after); j++ {
			if shown[j] {
				continue
			}
			shown[j] = true
			var cut bool
			view[j], cut = clipGrepLine(lines[j], nil, ctxGrepLineCap)
			clipped = clipped || cut
		}
	}
	matches := make([]map[string]any, 0, len(matchRows))
	for _, i := range matchRows {
		matches = append(matches, map[string]any{
			"line": i + 1,
			"text": view[i],
		})
	}
	return mcp.NewToolResultJSON(map[string]any{
		"response_id": e.ID,
		"tool":        e.Tool,
		"pattern":     pattern,
		"match_count": total,
		"returned":    len(matchRows),
		"truncated":   truncated,
		"total_lines":  len(lines),
		"matches":      matches,
		"line_clipped": clipped,
		"block":        grepBlock(view, matchRows, before, after),
	})
}

// ctxGrepLineCap bounds one echoed line; ctx_head/ctx_tail serve whole lines.
const ctxGrepLineCap = 512

// clipGrepLine shortens a line longer than limit bytes: around the first match
// of re when given, otherwise from its head, marking each cut end with "…".
func clipGrepLine(line string, re *regexp.Regexp, limit int) (string, bool) {
	if len(line) <= limit {
		return line, false
	}
	start := 0
	if re != nil {
		if loc := re.FindStringIndex(line); loc != nil {
			start = max(0, loc[0]-limit/4)
		}
	}
	end := min(len(line), start+limit)
	if end == len(line) {
		start = max(0, end-limit)
	}
	out := strings.ToValidUTF8(line[start:end], "")
	if start > 0 {
		out = "…" + out
	}
	if end < len(line) {
		out += "…"
	}
	return out, true
}

// grepBlock renders matched lines and their context the way grep -C
// does: `N:` prefixes a match line, `N-` a context line, and `--`
// separates non-adjacent groups. Overlapping context windows are
// merged. matchRows must be ascending.
func grepBlock(lines []string, matchRows []int, before, after int) string {
	if len(matchRows) == 0 {
		return ""
	}
	isMatch := make(map[int]bool, len(matchRows))
	for _, m := range matchRows {
		isMatch[m] = true
	}
	type span struct{ lo, hi int }
	var spans []span
	for _, m := range matchRows {
		lo, hi := m-before, m+after
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines)-1 {
			hi = len(lines) - 1
		}
		if n := len(spans); n > 0 && lo <= spans[n-1].hi+1 {
			if hi > spans[n-1].hi {
				spans[n-1].hi = hi
			}
		} else {
			spans = append(spans, span{lo, hi})
		}
	}
	var b strings.Builder
	for si, sp := range spans {
		if si > 0 {
			b.WriteString("--\n")
		}
		for i := sp.lo; i <= sp.hi; i++ {
			sep := "-"
			if isMatch[i] {
				sep = ":"
			}
			fmt.Fprintf(&b, "%d%s%s\n", i+1, sep, lines[i])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

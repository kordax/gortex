package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// These are unconditionally registered (single-project mode degrades
// to a one-member view) so an agent's first call into the server can
// discover what `repo` values are legal before issuing any
// scope: repo or scope: fan-out call.
func (s *Server) registerWorkspaceTools() {
	s.addTool(
		mcp.NewTool("list_repos",
			mcp.WithDescription(
				"Lists every project in the active workspace. Workspace-scope tool: do not pass `repo`. "+
					"In workspace mode returns the auto-discovered, non-excluded children. "+
					"In single-project mode returns the one bound project as a degenerate one-member workspace."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleListRepos,
	)

	s.addTool(
		mcp.NewTool("workspace_info",
			mcp.WithDescription(
				"Returns workspace identity: bind mode, root directory, marker contents, the auto-discovered member set, and any unknown marker keys. "+
					"Workspace-scope tool: do not pass `repo`."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleWorkspaceInfo,
	)
}

// handleListRepos implements scope: workspace's `list_repos`. Returns
// the auto-discovered, non-excluded member set. Single-project mode
// degrades to the one-member [bound project] list.
//
// Pre-handshake (Bind() == nil): returns an empty list rather than
// erroring. The MCP server may be running in legacy single-repo mode
// where no two-entry-point handshake has happened — in that case the
// concept of a workspace doesn't apply and an empty list is the
// honest answer.
func (s *Server) handleListRepos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Enforce scope: workspace's "no `repo`" rule explicitly so a
	// caller passing `repo` gets a clear protocol error instead of
	// silent acceptance.
	if _, errResult := s.ResolveToolScope("list_repos", req.GetArguments()["repo"]); errResult != nil {
		return errResult, nil
	}

	return s.respondJSONOrTOON(ctx, req, s.buildListReposPayload(ctx))
}

// buildListReposPayload returns the same data the `list_repos` tool
// emits. Shared with the `gortex://repos` resource.
//
// A workspace-bound session (daemon socket path) reports the repos in
// its own resolved workspace; an unbound session falls back to the
// process-global bind.
func (s *Server) buildListReposPayload(ctx context.Context) map[string]any {
	if sessWS, _, bound := s.sessionScope(ctx); bound {
		repos := s.sessionWorkspaceRepos(ctx)
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, r["name"])
		}
		return map[string]any{
			"mode":      "workspace",
			"workspace": sessWS,
			"repos":     names,
		}
	}
	// No session workspace (embedded single-repo server, or a control
	// client): there is no workspace boundary to report.
	return map[string]any{"mode": "unbound", "repos": []string{}}
}

// handleWorkspaceInfo implements `workspace_info`. Returns enough
// detail for an agent to reason about the bind: mode, root, marker
// excludes, marker unknown keys, and the resolved member set with
// per-member paths.
func (s *Server) handleWorkspaceInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if _, errResult := s.ResolveToolScope("workspace_info", req.GetArguments()["repo"]); errResult != nil {
		return errResult, nil
	}
	return s.respondJSONOrTOON(ctx, req, s.buildWorkspaceInfoPayload(ctx))
}

// buildWorkspaceInfoPayload returns the same data the `workspace_info`
// tool emits. Shared with the `gortex://workspace` resource.
//
// A workspace-bound session (daemon socket path) reports its own
// resolved workspace — the boundary the query tools enforce — instead
// of the process-global bind, which is nil on the daemon.
func (s *Server) buildWorkspaceInfoPayload(ctx context.Context) map[string]any {
	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		payload := map[string]any{
			"mode":             "workspace",
			"workspace":        sessWS,
			"project":          sessProj,
			"members":          s.sessionWorkspaceRepos(ctx),
			"isolation_bounds": sessWS,
		}
		// An explicit view of a sibling repository answers for that
		// repository; the cwd's own project is still reported beside it.
		if _, viewProj, ok := s.requestViewLocality(ctx, sessWS); ok && viewProj != sessProj {
			payload["project"] = viewProj
			payload["session_project"] = sessProj
		}
		return payload
	}
	// No session workspace: embedded single-repo / control client.
	return map[string]any{
		"mode":  "unbound",
		"repos": []map[string]string{},
	}
}

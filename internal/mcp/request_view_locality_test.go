package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionLocality_ExplicitViewOfSiblingRepo is the octopus / elk-client
// report: a session opened in one repository named
// view:{kind:"worktree"} for a sibling repository of the same workspace, and
// explore still ranked the cwd's repository first, while workspace.info
// reported the cwd's project beside an exact view of the sibling. Locality must
// follow the view the request reads through; the workspace boundary must not.
func TestSessionLocality_ExplicitViewOfSiblingRepo(t *testing.T) {
	s, root := newWorkspaceRootBoundServer(t)
	ctx := WithSessionCWD(context.Background(), filepath.Join(root, "r1"))

	home, _ := s.sessionLocality(ctx)
	require.Equal(t, "r1", home, "precondition: the cwd's repository is the session's home")

	viewed := withRequestView(ctx, &requestView{viewRoot: filepath.Join(root, "r2"), repoPrefix: "r2"})
	prefix, _ := s.sessionLocality(viewed)
	assert.Equal(t, "r2", prefix, "an explicit view of a sibling repository ranks that repository first")

	ws, _, _ := s.sessionScope(viewed)
	assert.Equal(t, "shared", ws, "the view must not move the session's workspace boundary")

	// A view of a repository in another workspace leaves locality alone: the
	// view gate refuses such a checkout, and ranking must not be the place
	// that lets it through.
	foreign := withRequestView(ctx, &requestView{viewRoot: s.multiIndexer.GetIndexer("z").RootPath(), repoPrefix: "z"})
	prefix, _ = s.sessionLocality(foreign)
	assert.Equal(t, "r1", prefix)
}

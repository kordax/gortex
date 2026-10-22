package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// agentCopyProjectSubdir takes a bounded, read-only snapshot of a directory in
// this checkout and places it under the private transport fixture repository.
type agentProjectSample struct {
	Path    string
	Content string
}

func agentCopyProjectSubdir(projectRoot, subdir, fixtureRoot string) ([]agentProjectSample, int, error) {
	if !filepath.IsLocal(subdir) || filepath.Clean(subdir) == "." {
		return nil, 0, fmt.Errorf("project subdirectory must be a relative path below the checkout: %q", subdir)
	}
	root, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return nil, 0, err
	}
	source, err := filepath.EvalSymlinks(filepath.Join(root, subdir))
	if err != nil {
		return nil, 0, err
	}
	relSource, err := filepath.Rel(root, source)
	if err != nil || !filepath.IsLocal(relSource) || relSource == "." {
		return nil, 0, fmt.Errorf("project subdirectory escapes the checkout: %q", subdir)
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("project subdirectory is not a directory: %q", subdir)
	}
	type sourceFile struct {
		path string
		data []byte
	}
	var files []sourceFile
	var samples []agentProjectSample
	var total int64
	const maxFiles = 2000
	const maxBytes = 64 << 20
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in project subdirectory: %s", path)
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file in project subdirectory: %s", path)
		}
		if len(files) >= maxFiles || info.Size() > maxBytes-total {
			return fmt.Errorf("project subdirectory exceeds %d files or %d bytes", maxFiles, maxBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > maxBytes {
			return fmt.Errorf("project subdirectory exceeds %d bytes", maxBytes)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(fixtureRoot, "project", relSource, rel)
		files = append(files, sourceFile{path: destination, data: data})
		if len(data) > 0 && len(data) <= 4096 && utf8.Valid(data) && len(samples) < 16 {
			samples = append(samples, agentProjectSample{Path: destination, Content: string(data)})
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(samples) == 0 {
		return nil, 0, fmt.Errorf("project subdirectory has no readable text file of at most 4096 bytes: %q", subdir)
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file.path), 0700); err != nil {
			return nil, 0, err
		}
		if err := os.WriteFile(file.path, file.data, 0600); err != nil {
			return nil, 0, err
		}
	}
	return samples, len(files), nil
}

func TestAgentCopyProjectSubdir(t *testing.T) {
	projectRoot := t.TempDir()
	fixtureRoot := t.TempDir()
	source := filepath.Join(projectRoot, "assets", "nested", "example.java")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0700))
	require.NoError(t, os.WriteFile(source, []byte("class Example {}\n"), 0600))
	samples, count, err := agentCopyProjectSubdir(projectRoot, filepath.Join("assets", "nested"), fixtureRoot)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, []agentProjectSample{{Path: filepath.Join(fixtureRoot, "project", "assets", "nested", "example.java"), Content: "class Example {}\n"}}, samples)
	copied, err := os.ReadFile(samples[0].Path)
	require.NoError(t, err)
	require.Equal(t, samples[0].Content, string(copied))
	original, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, samples[0].Content, string(original))
}

func TestAgentCopyProjectSubdirRejectsEscapesAndSymlinks(t *testing.T) {
	projectRoot := t.TempDir()
	fixtureRoot := t.TempDir()
	subdir := filepath.Join(projectRoot, "assets")
	require.NoError(t, os.MkdirAll(subdir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "example.go"), []byte("package fixture\n"), 0600))
	for _, path := range []string{".", "../other", fixtureRoot} {
		_, _, err := agentCopyProjectSubdir(projectRoot, path, fixtureRoot)
		require.Error(t, err, path)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(projectRoot, "outside")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, _, err := agentCopyProjectSubdir(projectRoot, "outside", fixtureRoot)
	require.Error(t, err)
	require.NoError(t, os.Symlink(filepath.Join(subdir, "example.go"), filepath.Join(subdir, "link.go")))
	_, _, err = agentCopyProjectSubdir(projectRoot, "assets", fixtureRoot)
	require.Error(t, err)
}

func TestAgentProjectSearchSource(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "repo", "project", "assets")
	good := map[string]any{"name": "UserService", "repo_prefix": "issue767", "absolute_file_path": filepath.Join(projectRoot, "UserService.java")}
	require.True(t, agentProjectSearchSource(map[string]any{"results": []any{good}}, "UserService", projectRoot))
	for _, bad := range []map[string]any{
		{"name": "UserService", "repo_prefix": "issue767", "absolute_file_path": filepath.Join(filepath.Dir(projectRoot), "other", "UserService.java")},
		{"name": "UserService", "repo_prefix": "other", "absolute_file_path": filepath.Join(projectRoot, "UserService.java")},
		{"name": "Other", "repo_prefix": "issue767", "absolute_file_path": filepath.Join(projectRoot, "UserService.java")},
	} {
		require.False(t, agentProjectSearchSource(map[string]any{"results": []any{bad}}, "UserService", projectRoot))
	}
}

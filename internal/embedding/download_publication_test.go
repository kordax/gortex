package embedding

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloadVerifiedConcurrentReplacement(t *testing.T) {
	const body = "replacement artifact verified before publication"
	dst := filepath.Join(t.TempDir(), "model")
	require.NoError(t, os.WriteFile(dst, []byte("previous artifact"), 0o600))
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- downloadVerifiedContext(ctx, server.Client(), server.URL, dst, downloadTestSHA(body)) }()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("concurrent local downloads did not start")
		}
	}
	unblock()
	for range 2 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("concurrent publication did not finish")
		}
	}
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
	partials, err := filepath.Glob(dst + ".partial*")
	require.NoError(t, err)
	assert.Empty(t, partials)
}

func TestDownloadVerifiedPublicationFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "model")
	require.NoError(t, os.Mkdir(dst, 0o700))
	sentinel := filepath.Join(dst, "existing")
	require.NoError(t, os.WriteFile(sentinel, []byte("preserve"), 0o600))
	const body = "verified artifact"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	require.Error(t, downloadVerifiedContext(t.Context(), server.Client(), server.URL, dst, downloadTestSHA(body)))
	got, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	assert.Equal(t, "preserve", string(got))
	partials, err := filepath.Glob(dst + ".partial*")
	require.NoError(t, err)
	assert.Empty(t, partials)
}

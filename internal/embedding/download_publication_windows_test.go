package embedding

import (
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

func TestDownloadVerifiedRetriesWindowsDestinationReader(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "model")
	require.NoError(t, os.WriteFile(dst, []byte("previous artifact"), 0o600))
	reader, err := os.Open(dst)
	require.NoError(t, err)
	release := sync.OnceFunc(func() { assert.NoError(t, reader.Close()) })
	t.Cleanup(release)
	const body = "verified replacement"
	served := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body); close(served) }))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- downloadVerifiedContext(t.Context(), server.Client(), server.URL, dst, downloadTestSHA(body))
	}()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local download did not start")
	}
	// Go's Windows reader does not share DELETE access. Atomic replacement must
	// retry while the old handle is open, without unlinking the destination.
	select {
	case err := <-done:
		t.Fatalf("publication finished while destination reader was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(4 * time.Second):
		t.Fatal("publication did not resume after reader closed")
	}
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
	partials, err := filepath.Glob(dst + ".partial*")
	require.NoError(t, err)
	assert.Empty(t, partials)
}

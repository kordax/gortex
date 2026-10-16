package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func downloadTestSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestDownloadVerifiedControlledHTTP(t *testing.T) {
	for _, mode := range []string{"delayed_success", "http_error", "checksum_error", "truncated_body", "cancel_headers", "cancel_body", "deadline_body"} {
		t.Run(mode, func(t *testing.T) {
			const body = "local pinned model fixture"
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			disconnected := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "cancel_headers" {
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
					close(disconnected)
					return
				}
				if mode == "cancel_body" || mode == "deadline_body" {
					_, _ = io.WriteString(w, "partial")
					w.(http.Flusher).Flush()
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
					close(disconnected)
					return
				}
				close(started)
				switch mode {
				case "delayed_success":
					<-release
				case "http_error":
					http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
					return
				case "truncated_body":
					w.Header().Set("Content-Length", "1000")
				}
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(unblock)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline_body" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			dst := filepath.Join(t.TempDir(), "model")
			require.NoError(t, os.WriteFile(dst, []byte("original"), 0o600))
			wantSHA := downloadTestSHA(body)
			if mode == "checksum_error" {
				wantSHA = downloadTestSHA("different")
			}
			done := make(chan error, 1)
			go func() { done <- downloadVerifiedContext(ctx, server.Client(), server.URL, dst, wantSHA) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("download did not reach the local server")
			}
			if mode == "cancel_headers" || mode == "cancel_body" {
				cancel()
			}
			if mode == "delayed_success" {
				select {
				case err := <-done:
					t.Fatalf("download completed before fixture released it: %v", err)
				default:
				}
				unblock()
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("download failed to finish after release or cancellation")
			}
			if mode == "delayed_success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "cancel_headers" || mode == "cancel_body" {
				assert.ErrorIs(t, err, context.Canceled)
			}
			if mode == "deadline_body" {
				assert.ErrorIs(t, err, context.DeadlineExceeded)
			}
			if mode == "cancel_headers" || mode == "cancel_body" || mode == "deadline_body" {
				select {
				case <-disconnected:
				case <-time.After(3 * time.Second):
					t.Fatal("server did not observe request cancellation")
				}
			}
			got, readErr := os.ReadFile(dst)
			require.NoError(t, readErr)
			if mode == "delayed_success" {
				assert.Equal(t, body, string(got))
			} else {
				assert.Equal(t, "original", string(got), "failed download replaced the cached model")
			}
			partials, globErr := filepath.Glob(dst + ".partial*")
			require.NoError(t, globErr)
			assert.Empty(t, partials)
		})
	}
}

func TestDownloadVerifiedConcurrentDestination(t *testing.T) {
	const body = "same verified artifact"
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body[:4])
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-release
		_, _ = io.WriteString(w, body[4:])
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	dst := filepath.Join(t.TempDir(), "model")
	done := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 {
		go func() { done <- downloadVerifiedContext(ctx, server.Client(), server.URL, dst, downloadTestSHA(body)) }()
	}
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("concurrent downloads did not start")
		}
	}
	unblock()
	for range 2 {
		require.NoError(t, <-done)
	}
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
	partials, err := filepath.Glob(dst + ".partial*")
	require.NoError(t, err)
	assert.Empty(t, partials)
}

func waitForEmbedderWaiters(t *testing.T, cache *codeEmbedderCache, count int) *codeEmbedderFlight {
	t.Helper()
	var flight *codeEmbedderFlight
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		flight = cache.flight
		return flight != nil && flight.waiters == count
	}, 3*time.Second, time.Millisecond)
	return flight
}

func TestCodeEmbedderColdLoadIndependentWaiters(t *testing.T) {
	cache := &codeEmbedderCache{enabled: true}
	t.Cleanup(func() { cache.setEnabled(false) })
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, "model")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	provider := &PotionProvider{dims: 1}
	dst := filepath.Join(t.TempDir(), "model")
	load := func(ctx context.Context) Provider {
		if err := downloadVerifiedContext(ctx, server.Client(), server.URL, dst, downloadTestSHA("model")); err != nil {
			return nil
		}
		return provider
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSecond()
	first, second := make(chan Provider, 1), make(chan Provider, 1)
	go func() { first <- cache.get(firstCtx, load) }()
	select {
	case <-started:
	case <-secondCtx.Done():
		t.Fatal("cold load did not start")
	}
	go func() { second <- cache.get(secondCtx, load) }()
	waitForEmbedderWaiters(t, cache, 2)
	cancelFirst()
	select {
	case got := <-first:
		assert.Nil(t, got)
	case <-secondCtx.Done():
		t.Fatal("canceled waiter did not return")
	}
	unblock()
	select {
	case got := <-second:
		assert.Same(t, provider, got)
	case <-secondCtx.Done():
		t.Fatal("healthy waiter did not receive the shared load")
	}
	assert.Same(t, provider, cache.get(secondCtx, load))
	assert.EqualValues(t, 1, calls.Load(), "cold callers downloaded more than once")
}

func TestCodeEmbedderColdLoadCancellationAndDisable(t *testing.T) {
	for _, mode := range []string{"last_waiter", "disable"} {
		t.Run(mode, func(t *testing.T) {
			cache := &codeEmbedderCache{enabled: true}
			t.Cleanup(func() { cache.setEnabled(false) })
			requestCanceled := make(chan struct{})
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "partial")
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-r.Context().Done():
					close(requestCanceled)
				case <-release:
				}

			}))
			t.Cleanup(server.Close)
			// Also unblock a regressed contextless download when assertions fail.
			t.Cleanup(func() { close(release) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dst := filepath.Join(t.TempDir(), "model")
			done := make(chan Provider, 1)
			go func() {
				done <- cache.get(ctx, func(ctx context.Context) Provider {
					_ = downloadVerifiedContext(ctx, server.Client(), server.URL, dst, downloadTestSHA("model"))
					return &PotionProvider{dims: 1}
				})
			}()
			flight := waitForEmbedderWaiters(t, cache, 1)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("download did not reach the local server")
			}
			if mode == "last_waiter" {
				cancel()
			} else {
				disabled := make(chan struct{})
				go func() { cache.setEnabled(false); close(disabled) }()
				select {
				case <-disabled:
				case <-time.After(3 * time.Second):
					t.Fatal("configuration blocked behind a cold download")
				}
			}
			select {
			case got := <-done:
				assert.Nil(t, got)
			case <-time.After(3 * time.Second):
				t.Fatal("waiter failed to return")
			}
			select {
			case <-flight.done:
			case <-time.After(3 * time.Second):
				t.Fatal("abandoned cold loader did not exit")
			}
			select {
			case <-requestCanceled:
			case <-time.After(3 * time.Second):
				t.Fatal("server did not observe abandoned download cancellation")
			}
			cache.setEnabled(true)
			provider := &PotionProvider{dims: 2}
			assert.Same(t, provider, cache.get(context.Background(), func(context.Context) Provider { return provider }))
		})
	}
}

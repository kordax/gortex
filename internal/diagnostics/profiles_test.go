package diagnostics

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalDiagnosticsBoundary(t *testing.T) {
	for _, test := range []struct {
		name, host, origin, site, method string
		status                           int
	}{
		{"loopback", "127.0.0.1:9475", "", "", "GET", 204},
		{"ipv6", "[::1]:9475", "", "", "GET", 204},
		{"localhost", "localhost:9475", "", "none", "GET", 204},
		{"dns_rebinding", "attacker.example:9475", "", "", "GET", 403},
		{"browser", "127.0.0.1:9475", "https://attacker.example", "", "GET", 403},
		{"cross_site", "127.0.0.1:9475", "", "cross-site", "GET", 403},
		{"post", "127.0.0.1:9475", "", "", "POST", 405},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://127.0.0.1/", nil)
			r.Host = test.host
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.site)
			w := httptest.NewRecorder()
			LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			assert.Equal(t, test.status, w.Code)
		})
	}
	for _, address := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0", "attacker.example:0", "invalid"} {
		_, err := ListenLocal(address)
		require.Error(t, err, address)
	}
	listener, err := ListenLocal("localhost:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestProfileBoundsAndSnapshot(t *testing.T) {
	handler := ProfileHandler()
	for _, path := range []string{"cpu?seconds=0", "cpu?seconds=61", "cpu?seconds=oops", "trace?seconds=31", "block?seconds=31", "heap?debug=1"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/"+path, nil))
		assert.Equal(t, http.StatusBadRequest, w.Code, path)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/goroutine", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Greater(t, w.Body.Len(), 2)
	assert.Equal(t, []byte{0x1f, 0x8b}, w.Body.Bytes()[:2], "Go profiles must be gzip protobuf")
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
}

func TestProfileTextGoroutineDumpCompatibility(t *testing.T) {
	w := httptest.NewRecorder()
	ProfileHandler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/goroutine?debug=2", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")
	assert.Contains(t, w.Body.String(), "goroutine ")
}

func TestProfileCancellationReleasesCaptureGate(t *testing.T) {
	gate := make(chan struct{}, 1)
	handler := profileHandlerWithGates(gate, make(chan struct{}, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/cpu?seconds=60", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	require.Eventually(t, func() bool { return len(gate) == 1 }, time.Second, 10*time.Millisecond)
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/heap", nil))
	require.Equal(t, http.StatusConflict, busy.Code)
	dump := httptest.NewRecorder()
	handler.ServeHTTP(dump, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/goroutine?debug=2", nil))
	assert.Equal(t, http.StatusOK, dump.Code, "goroutine diagnostics remain available during a CPU capture")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled CPU capture did not stop")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/debug/pprof/heap", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProfileBufferBoundAndMutexRestoration(t *testing.T) {
	buffer := boundedProfile{Buffer: *bytes.NewBuffer(make([]byte, maxProfileBytes-1))}
	written, err := buffer.Write([]byte("excess"))
	require.NoError(t, err)
	assert.Equal(t, 6, written)
	assert.Equal(t, maxProfileBytes, buffer.Len())
	assert.True(t, buffer.overflow)
	previous := runtime.SetMutexProfileFraction(7)
	defer runtime.SetMutexProfileFraction(previous)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, captureProfile(ctx, "mutex", time.Second, &bytes.Buffer{}), context.DeadlineExceeded)
	assert.Equal(t, 7, runtime.SetMutexProfileFraction(-1))
}

package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnavailableTraceCollectorDoesNotBackpressureOperations(t *testing.T) {
	var once sync.Once
	release := make(chan struct{})
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	defer unblock()
	shutdown, err := StartTracing(context.Background(), TraceConfig{Endpoint: server.URL + "/v1/traces", Sampler: "always_on"})
	require.NoError(t, err)
	defer func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5000 {
			_, finish := TraceOperation(context.Background(), "test", "bounded_queue")
			finish(nil)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("trace queue backpressured application operations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
	require.Nil(t, activeTracing.Load(), "shutdown must disconnect instrumentation even while export is stalled")
	unblock()
}

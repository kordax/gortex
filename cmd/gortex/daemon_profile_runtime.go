package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/diagnostics"
	"go.uber.org/zap"
)

// daemonProfiler owns an opt-in listener. A stopped listener cannot admit new
// captures, and cannot be replaced until all its handlers have released sampling.
type daemonProfiler struct {
	mu        sync.Mutex
	ctx       context.Context
	logger    *zap.Logger
	preferred string
	handler   http.Handler
	current   *daemonProfileListener
	closed    bool
}

type daemonProfileListener struct {
	server    *http.Server
	listener  net.Listener
	requested string
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	stopping  bool
	active    sync.WaitGroup
}

func (c *realController) Profiler(ctx context.Context, p daemon.ProfilerParams) (daemon.ProfilerStatus, error) {
	if c.profiler == nil {
		return daemon.ProfilerStatus{}, fmt.Errorf("profiler control is unavailable")
	}
	return c.profiler.apply(ctx, p)
}

func (p *daemonProfileListener) stop() {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.cancel()
	_ = p.server.Close()
	// Serve may not have registered the listener yet.
	_ = p.listener.Close()
}

func (p *daemonProfileListener) wrap(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if p.stopping {
			p.mu.Unlock()
			http.Error(w, "profiler is stopping", http.StatusServiceUnavailable)
			return
		}
		p.active.Add(1)
		p.mu.Unlock()
		defer p.active.Done()
		handler.ServeHTTP(w, r)
	})
}

func (d *daemonProfiler) statusLocked() daemon.ProfilerStatus {
	p := d.current
	if p == nil {
		return daemon.ProfilerStatus{}
	}
	select {
	case <-p.done:
		d.current = nil
		pprofAddr.Store("")
		return daemon.ProfilerStatus{}
	default:
	}
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	address := p.listener.Addr().String()
	return daemon.ProfilerStatus{Running: !stopping, Stopping: stopping, Address: address}
}

func (d *daemonProfiler) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if d.mu.TryLock() {
				return nil
			}
		}
	}
}

func (d *daemonProfiler) apply(ctx context.Context, params daemon.ProfilerParams) (daemon.ProfilerStatus, error) {
	if err := d.lock(ctx); err != nil {
		return daemon.ProfilerStatus{}, err
	}
	defer d.mu.Unlock()
	status := d.statusLocked()
	if err := ctx.Err(); err != nil {
		return status, err
	}
	if params.Action != "start" && params.Action != "stop" && params.Action != "status" {
		return status, fmt.Errorf("unknown profiler action %q", params.Action)
	}
	if params.Address != "" && params.Action != "start" {
		return status, fmt.Errorf("address is only valid for profiler start")
	}
	if params.Action == "status" {
		return status, nil
	}
	if params.Action == "stop" {
		if d.current == nil {
			return status, nil
		}
		d.current.stop()
		pprofAddr.Store("")
		cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		select {
		case <-d.current.done:
			return d.statusLocked(), nil
		case <-cleanupCtx.Done():
			return d.statusLocked(), fmt.Errorf("profiler capture cleanup is still finishing: %w", cleanupCtx.Err())
		}
	}
	if d.closed || d.ctx.Err() != nil {
		return status, fmt.Errorf("daemon observability is shutting down")
	}
	if status.Stopping {
		return status, fmt.Errorf("previous profiler capture is still stopping")
	}
	if status.Running {
		if params.Address != "" && params.Address != d.current.requested && params.Address != status.Address {
			return status, fmt.Errorf("profiler is already running at %s; stop it before changing address", status.Address)
		}
		return status, nil
	}
	address := params.Address
	if address == "" {
		address = d.preferred
	}
	if address == "" {
		address = "127.0.0.1:9475"
	}
	listener, err := diagnostics.ListenLocal(address)
	if err != nil {
		return status, err
	}
	captureCtx, cancel := context.WithCancel(d.ctx)
	p := &daemonProfileListener{listener: listener, requested: address, cancel: cancel, done: make(chan struct{})}
	handler := d.handler
	if handler == nil {
		handler = diagnostics.ProfileHandler()
	}
	p.server = &http.Server{
		Handler: p.wrap(handler), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		WriteTimeout: 70 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return captureCtx },
	}
	d.current = p
	pprofAddr.Store(listener.Addr().String())
	d.logger.Info("daemon: local observability listener", zap.String("kind", "profiles"), zap.String("address", listener.Addr().String()))
	go func() {
		err := p.server.Serve(listener)
		p.stop()
		// stop closes admission before Wait, so no Add can race this wait.
		p.active.Wait()
		close(p.done)
		if err != nil && err != http.ErrServerClosed {
			d.logger.Warn("daemon: profiler listener stopped")
		}
	}()
	return d.statusLocked(), nil
}

func (d *daemonProfiler) close() {
	d.mu.Lock()
	d.closed = true
	p := d.current
	if p != nil {
		p.stop()
	}
	pprofAddr.Store("")
	d.mu.Unlock()
	if p == nil {
		return
	}
	// Shutdown always closes admission and the port; only cleanup is timed.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		d.logger.Warn("daemon: profiler capture cleanup is still finishing")
	}
}

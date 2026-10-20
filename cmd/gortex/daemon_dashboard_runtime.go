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

// daemonDashboard owns only HTTP serving; stopping it leaves metric history alive.
type daemonDashboard struct {
	mu        sync.Mutex
	ctx       context.Context
	handler   http.Handler
	logger    *zap.Logger
	preferred string
	requested string
	address   string
	server    *http.Server
	listener  net.Listener
	closed    bool
}

func (c *realController) Dashboard(ctx context.Context, p daemon.DashboardParams) (daemon.DashboardStatus, error) {
	if c.dashboard == nil {
		return daemon.DashboardStatus{}, fmt.Errorf("dashboard control is unavailable")
	}
	return c.dashboard.apply(ctx, p)
}

func (d *daemonDashboard) statusLocked() daemon.DashboardStatus {
	if d.server == nil {
		return daemon.DashboardStatus{}
	}
	return daemon.DashboardStatus{Running: true, Address: d.address, URL: "http://" + d.address}
}

func (d *daemonDashboard) apply(ctx context.Context, p daemon.DashboardParams) (daemon.DashboardStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return d.statusLocked(), err
	}
	if p.Action != "start" && p.Action != "stop" && p.Action != "status" {
		return d.statusLocked(), fmt.Errorf("unknown dashboard action %q", p.Action)
	}
	if p.Address != "" && p.Action != "start" {
		return d.statusLocked(), fmt.Errorf("address is only valid for dashboard start")
	}
	if p.Action == "status" {
		return d.statusLocked(), nil
	}
	if p.Action == "stop" {
		return daemon.DashboardStatus{}, d.stopLocked()
	}
	if d.closed || d.ctx.Err() != nil {
		return d.statusLocked(), fmt.Errorf("daemon observability is shutting down")
	}
	if d.server != nil {
		if p.Address != "" && p.Address != d.requested && p.Address != d.address {
			return d.statusLocked(), fmt.Errorf("dashboard is already running at %s; stop it before changing address", d.address)
		}
		return d.statusLocked(), nil
	}
	address := p.Address
	if address == "" {
		address = d.preferred
	}
	if address == "" {
		address = "127.0.0.1:9474"
	}
	listener, err := diagnostics.ListenLocal(address)
	if err != nil {
		return d.statusLocked(), fmt.Errorf("dashboard listener: %w", err)
	}
	server := &http.Server{
		Handler: d.handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		WriteTimeout: 70 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return d.ctx },
	}
	d.server, d.listener, d.requested, d.address = server, listener, address, listener.Addr().String()
	d.logger.Info("daemon: local observability listener", zap.String("kind", "metrics"), zap.String("address", d.address))
	go func() {
		err := server.Serve(listener)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.server == server {
			d.server, d.listener, d.address, d.requested = nil, nil, "", ""
		}
		if err != nil && err != http.ErrServerClosed {
			d.logger.Warn("daemon: observability listener stopped", zap.String("kind", "metrics"))
		}
	}()
	return d.statusLocked(), nil
}

func (d *daemonDashboard) stopLocked() error {
	if d.server == nil {
		return nil
	}
	err := d.server.Close()
	// Close also before Serve has registered the listener with http.Server.
	// This makes an immediate start/stop release the port synchronously.
	_ = d.listener.Close()
	d.server, d.listener, d.address, d.requested = nil, nil, "", ""
	return err
}

func (d *daemonDashboard) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	_ = d.stopLocked()
}

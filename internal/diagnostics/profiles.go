// Package diagnostics provides local, opt-in tracing and bounded Go profiles.
package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/pprof"
	runtimetrace "runtime/trace"
	"strconv"
	"strings"
	"time"
)

const maxProfileBytes = 32 << 20

// LocalOnly rejects browser origins and non-loopback Hosts, including DNS rebinding.
// It complements a loopback listener; it is not authentication for a public bind.
func LocalOnly(next http.Handler) http.Handler {
	return localGuard(next, false)
}

// LocalSameOrigin is LocalOnly for a page served from the listener itself: it
// also admits the requests that page makes for its own assets and data, which a
// browser marks same-origin. Every other site, and every non-loopback Host, is
// still refused.
func LocalSameOrigin(next http.Handler) http.Handler {
	return localGuard(next, true)
}

func localGuard(next http.Handler, sameOrigin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback || !localBrowserAllowed(r, sameOrigin) {
			http.Error(w, "local diagnostics only", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localBrowserAllowed admits requests no browser page initiated and, when
// sameOrigin is set, requests from a page this very Host served.
func localBrowserAllowed(r *http.Request, sameOrigin bool) bool {
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if origin == "" && (site == "" || site == "none") {
		return true
	}
	if !sameOrigin {
		return false
	}
	if site != "" && site != "same-origin" {
		return false
	}
	return origin == "" || origin == "http://"+r.Host
}

// ListenLocal requires a literal loopback address (or localhost), never a wildcard.
func ListenLocal(address string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("diagnostic address must be host:port: %w", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("diagnostic address must use a loopback IP or localhost")
	}
	return net.Listen("tcp", net.JoinHostPort(host, port))
}

// ProfileHandler keeps costly profiles mutually exclusive and bounded. It does
// not register on http.DefaultServeMux or enable continuous mutex/block sampling.
func ProfileHandler() http.Handler {
	return profileHandlerWithGates(make(chan struct{}, 1), make(chan struct{}, 1))
}

func profileHandlerWithGates(gate, goroutineGate chan struct{}) http.Handler {
	mux := http.NewServeMux()
	for _, name := range []string{"heap", "allocs", "goroutine", "threadcreate", "cpu", "profile", "trace", "mutex", "block"} {
		mux.HandleFunc("/debug/pprof/"+name, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				for key := range r.URL.Query() {
					if key != "seconds" && (key != "debug" || name != "goroutine") {
						http.Error(w, "only seconds is supported", http.StatusBadRequest)
						return
					}
				}
			}
			kind := name
			textDump := false
			if raw := r.URL.Query().Get("debug"); raw != "" && raw != "0" {
				if raw != "1" && raw != "2" {
					http.Error(w, "goroutine debug must be 0, 1 or 2", http.StatusBadRequest)
					return
				}
				kind = "goroutine_debug" + raw
				textDump = true
			}
			if kind == "profile" {
				kind = "cpu"
			}
			duration := time.Duration(0)
			if kind == "cpu" || kind == "trace" || kind == "mutex" || kind == "block" {
				seconds := 10
				if raw := r.URL.Query().Get("seconds"); raw != "" {
					value, err := strconv.Atoi(raw)
					if err != nil {
						http.Error(w, "seconds must be an integer", http.StatusBadRequest)
						return
					}
					seconds = value
				}
				maximum := 30
				if kind == "cpu" {
					maximum = 60
				}
				if seconds < 1 || seconds > maximum {
					http.Error(w, fmt.Sprintf("seconds must be between 1 and %d", maximum), http.StatusBadRequest)
					return
				}
				duration = time.Duration(seconds) * time.Second
			}
			captureGate := gate
			if strings.HasPrefix(kind, "goroutine") {
				captureGate = goroutineGate
			}
			select {
			case captureGate <- struct{}{}:
				defer func() { <-captureGate }()
			default:
				http.Error(w, "another profile is active", http.StatusConflict)
				return
			}
			var output boundedProfile
			profileCtx, finish := TraceOperation(r.Context(), "profile", kind)
			err := captureProfile(profileCtx, kind, duration, &output)
			finish(err)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				http.Error(w, "profile unavailable", http.StatusConflict)
				return
			}
			if output.overflow {
				http.Error(w, "profile exceeds 32 MiB; use a shorter capture", http.StatusRequestEntityTooLarge)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pprof"`)
			if textDump {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Content-Disposition", `attachment; filename="goroutine.txt"`)
			}
			w.Header().Set("Content-Length", strconv.Itoa(output.Len()))
			_, _ = w.Write(output.Bytes())
		})
	}
	return LocalOnly(mux)
}

type boundedProfile struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedProfile) Write(p []byte) (int, error) {
	accepted := len(p)
	remaining := maxProfileBytes - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	// Profilers keep writing until stopped. Discard beyond the cap, then return
	// an HTTP error, rather than allocating an unbounded buffer.
	return accepted, nil
}

func captureProfile(ctx context.Context, kind string, duration time.Duration, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch kind {
	case "cpu":
		if err := pprof.StartCPUProfile(output); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	case "trace":
		if err := runtimetrace.Start(output); err != nil {
			return err
		}
		defer runtimetrace.Stop()
	case "mutex":
		previous := runtime.SetMutexProfileFraction(10)
		defer runtime.SetMutexProfileFraction(previous)
	case "block":
		// The daemon does not enable block profiling elsewhere. Sample at 1ms
		// only for this capture, then disable it again.
		runtime.SetBlockProfileRate(1_000_000)
		defer runtime.SetBlockProfileRate(0)
	}
	if duration > 0 {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if kind == "cpu" || kind == "trace" {
		return nil
	}
	debug := 0
	if kind == "goroutine_debug1" || kind == "goroutine_debug2" {
		debug, _ = strconv.Atoi(strings.TrimPrefix(kind, "goroutine_debug"))
		kind = "goroutine"
	}
	profile := pprof.Lookup(kind)
	if profile == nil {
		return fmt.Errorf("unknown profile kind")
	}
	return profile.WriteTo(output, debug)
}

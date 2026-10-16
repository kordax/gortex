package embedding

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
)

// sharedStatic memoises a single process-wide StaticProvider. The baked
// GloVe vectors are ~3.7MB compressed and decompress into a ~20k-entry
// map that is safe for concurrent reads, so one instance serves every
// rerank call. Constructed lazily on first use.
var (
	sharedStaticOnce sync.Once
	sharedStaticInst *StaticProvider
)

// SharedStatic returns the process-wide static word-vector provider,
// constructing it on first call. Returns nil only when the baked
// vectors fail to load (a corrupt build); callers treat nil as "no
// semantic-cosine channel". Safe for concurrent use.
func SharedStatic() *StaticProvider {
	sharedStaticOnce.Do(func() {
		p, err := NewStaticProvider()
		if err != nil {
			return
		}
		sharedStaticInst = p
	})
	return sharedStaticInst
}

// EmbedTextFunc adapts a provider into the plain func the rerank
// Context wants: text -> normalised vector, errors and nil providers
// collapsing to a nil result the signal reads as "cannot embed".
func EmbedTextFunc(p Provider) func(string) []float32 {
	if p == nil {
		return nil
	}
	return func(text string) []float32 {
		vec, err := p.Embed(context.Background(), text)
		if err != nil {
			return nil
		}
		return vec
	}
}

// sharedCode holds the process-wide code-embedding provider used by the
// rerank's semantic-cosine channel: the bundled static code model (potion)
// when its files resolve — explicit dir, exec-adjacent sidecar, per-user
// models dir, or a checksum-verified first-use download — and the baked
// GloVe word vectors otherwise, so an offline install without the sidecar
// still gets a semantic channel.
//
// It is deliberately NOT a sync.Once. The potion matrix is tens of MiB and
// a daemon that stops searching should not hold it for the rest of its
// life: the loader records when it was last used and a reaper drops it
// after an idle period, so the next search pays a reload instead of the
// process paying rent forever.
var sharedCode = codeEmbedderCache{enabled: true}

// A flight belongs to its waiters collectively. Canceling one request must
// not interrupt another request still waiting for the same cold load.
type codeEmbedderFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
}

type codeEmbedderCache struct {
	mu       sync.Mutex
	enabled  bool
	loaded   bool
	provider Provider
	flight   *codeEmbedderFlight
	usedAt   time.Time
	reaper   *time.Timer
}

// codeEmbedderIdleTTL is how long the code embedder survives with no
// rerank traffic. Long enough that a working session never reloads,
// short enough that an idle daemon gives the memory back.
const codeEmbedderIdleTTL = 30 * time.Minute

// SetCodeEmbedderEnabled turns the rerank semantic-cosine channel on or
// off process-wide, and drops an already-loaded model when turning it off.
// The daemon calls this from its resolved configuration; the channel is on
// by default, which is the historical behaviour.
func SetCodeEmbedderEnabled(enabled bool) {
	sharedCode.setEnabled(enabled)
}

func (c *codeEmbedderCache) setEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	if !enabled {
		c.provider = nil
		c.loaded = false
		if c.flight != nil {
			c.flight.cancel()
		}
		if c.reaper != nil {
			c.reaper.Stop()
			c.reaper = nil
		}
	}
}

// SharedCodeEmbedder returns the process-wide code embedder for the
// rerank's semantic-cosine channel, loading it on first use and reloading
// it after an idle eviction. Never returns an error — the fallback chain
// ends at the baked static provider; nil only when even that failed to
// load, or when the channel is disabled. Safe for concurrent use.
// GORTEX_POTION=0 pins the GloVe fallback (diagnostic escape hatch).
func SharedCodeEmbedder() Provider {
	return SharedCodeEmbedderContext(context.Background())
}

// SharedCodeEmbedderContext waits for the shared cold load until ctx ends.
// The last departing waiter cancels an unfinished download. Loading never
// holds the cache mutex, so configuration changes remain responsive.
func SharedCodeEmbedderContext(ctx context.Context) Provider {
	return sharedCode.get(ctx, buildCodeEmbedderContext)
}

func (c *codeEmbedderCache) get(ctx context.Context, load func(context.Context) Provider) Provider {
	for ctx.Err() == nil {
		c.mu.Lock()
		if !c.enabled {
			c.mu.Unlock()
			return nil
		}
		c.usedAt = time.Now()
		if c.loaded {
			p := c.provider
			c.mu.Unlock()
			return p
		}
		f := c.flight
		if f == nil {
			loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			f = &codeEmbedderFlight{done: make(chan struct{}), cancel: cancel}
			c.flight = f
			go c.load(loadCtx, f, load)
		}
		f.waiters++
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			c.mu.Lock()
			f.waiters--
			if f.waiters == 0 {
				f.cancel()
			}
			c.mu.Unlock()
			return nil
		case <-f.done:
			// A disabled or canceled flight is not cached. Retry only if
			// this request is still live and the channel remains enabled.
		}
	}
	return nil
}

func (c *codeEmbedderCache) load(ctx context.Context, f *codeEmbedderFlight, load func(context.Context) Provider) {
	defer f.cancel()
	p := load(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() == nil && c.enabled {
		c.provider = p
		c.loaded = true
		c.usedAt = time.Now()
		if c.reaper == nil {
			c.reaper = time.AfterFunc(codeEmbedderIdleTTL, c.reap)
		}
	}
	c.flight = nil
	close(f.done)
}

func buildCodeEmbedderContext(ctx context.Context) Provider {
	if ctx.Err() != nil {
		return nil
	}
	if v := strings.TrimSpace(os.Getenv("GORTEX_POTION")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		return staticOrNil()
	}
	dir := resolvePotionDir()
	if dir == "" {
		if d, err := downloadPotionContext(ctx); err == nil {
			dir = d
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if dir != "" {
		if p, err := NewPotionProviderFromDir(dir); err == nil {
			return p
		}
	}
	return staticOrNil()
}

// reap retains no periodic goroutine while the channel is disabled.
func (c *codeEmbedderCache) reap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.reaper = nil
		return
	}
	if remaining := codeEmbedderIdleTTL - time.Since(c.usedAt); remaining > 0 {
		c.reaper.Reset(remaining)
		return
	}
	if closer, ok := c.provider.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	c.provider = nil
	c.loaded = false
	if c.reaper != nil {
		c.reaper.Stop()
	}
	c.reaper = nil
}

// staticOrNil adapts SharedStatic's concrete return into the Provider
// interface without wrapping a typed nil.
func staticOrNil() Provider {
	if p := SharedStatic(); p != nil {
		return p
	}
	return nil
}

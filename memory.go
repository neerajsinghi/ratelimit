package ratelimit

import (
	"context"
	"sync"
	"time"
)

// sweepEvery is how many Allow calls happen between sweeps of expired keys.
const sweepEvery = 1024

// MemoryLimiter is an in-process Limiter. It implements exactly the same
// algorithms as RedisLimiter and is safe for concurrent use, but its state is
// local to one process. Use it for tests, development, or single-instance
// services.
type MemoryLimiter struct {
	limit Limit
	cfg   config

	mu      sync.Mutex
	entries map[string]*memEntry
	calls   int
}

type memEntry struct {
	gcra      gcraState
	sw        swState
	expiresAt int64 // microseconds
}

var _ Limiter = (*MemoryLimiter)(nil)

// NewMemory returns an in-memory limiter enforcing limit. It panics if limit
// is invalid (fewer than one request, or a period shorter than one
// microsecond per request).
func NewMemory(limit Limit, opts ...Option) *MemoryLimiter {
	if err := limit.validate(); err != nil {
		panic(err)
	}
	cfg := newConfig(opts)
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &MemoryLimiter{limit: limit, cfg: cfg, entries: make(map[string]*memEntry)}
}

// Limit returns the limit enforced by l.
func (l *MemoryLimiter) Limit() Limit { return l.limit }

// Allow implements Limiter.
func (l *MemoryLimiter) Allow(ctx context.Context, key string, n int) (Result, error) {
	if err := checkCost(n, l.limit); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	now := l.cfg.now().UnixMicro()
	limit, period, cost := int64(l.limit.Requests), l.limit.Period.Microseconds(), int64(n)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls++
	if l.calls%sweepEvery == 0 {
		l.sweep(now)
	}

	e, ok := l.entries[key]
	if ok && e.expiresAt <= now {
		delete(l.entries, key)
		e, ok = nil, false
	}
	var r raw
	switch l.cfg.algorithm {
	case SlidingWindow:
		var st swState
		if ok {
			st = e.sw
		}
		var store bool
		var ttl int64
		r, st, store, ttl = swStep(limit, period, cost, now, st, ok)
		if store {
			l.entries[key] = &memEntry{sw: st, expiresAt: now + ttl}
		}
	default:
		var st gcraState
		if ok {
			st = e.gcra
		}
		var store bool
		r, st, store = gcraStep(limit, period, cost, now, st, ok)
		if store {
			l.entries[key] = &memEntry{gcra: st, expiresAt: now + r.resetAfter}
		}
	}
	return makeResult(r, l.limit), nil
}

// Reset forgets all state for key.
func (l *MemoryLimiter) Reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// sweep removes expired entries. l.mu must be held.
func (l *MemoryLimiter) sweep(now int64) {
	for k, e := range l.entries {
		if e.expiresAt <= now {
			delete(l.entries, k)
		}
	}
}

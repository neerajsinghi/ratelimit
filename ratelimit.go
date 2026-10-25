// Package ratelimit provides distributed rate limiting backed by Redis, with
// an in-memory implementation for tests and single-instance services.
//
// Two algorithms are available:
//
//   - GCRA (Generic Cell Rate Algorithm): a smooth, burst-tolerant token
//     bucket equivalent that stores a single timestamp per key.
//   - SlidingWindow: a sliding-window counter that weights the previous fixed
//     window by its overlap with the sliding window, storing two counters per
//     key.
//
// The Redis implementations run as atomic Lua scripts (see the scripts/
// directory). The same scripts are used by the companion NestJS package
// (@neerajsinghi/nest-ratelimit), so Go and Node.js services sharing a Redis
// instance enforce the same limits with the same semantics.
//
// Basic usage:
//
//	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	limiter := ratelimit.NewRedis(rdb, ratelimit.PerMinute(100))
//	http.Handle("/", ratelimit.Middleware(limiter)(handler))
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Limiter decides whether an action identified by key may proceed.
type Limiter interface {
	// Allow reports whether n units (usually requests) may be consumed for
	// key right now. When the result is not allowed nothing is consumed.
	// n == 0 inspects the current state without consuming anything.
	Allow(ctx context.Context, key string, n int) (Result, error)
}

// Result describes the outcome of a call to Limiter.Allow.
type Result struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the number of requests allowed per period.
	Limit int
	// Remaining is how many more units could be consumed immediately.
	Remaining int
	// RetryAfter is how long to wait before the same request would be
	// allowed. It is zero when Allowed is true.
	RetryAfter time.Duration
	// ResetAfter is how long until the limiter is back to its full,
	// untouched state for this key.
	ResetAfter time.Duration
}

// Limit is a rate expressed as Requests per Period.
type Limit struct {
	Requests int
	Period   time.Duration
}

// PerSecond returns a limit of n requests per second.
func PerSecond(n int) Limit { return Limit{Requests: n, Period: time.Second} }

// PerMinute returns a limit of n requests per minute.
func PerMinute(n int) Limit { return Limit{Requests: n, Period: time.Minute} }

// PerHour returns a limit of n requests per hour.
func PerHour(n int) Limit { return Limit{Requests: n, Period: time.Hour} }

// String implements fmt.Stringer.
func (l Limit) String() string { return fmt.Sprintf("%d/%s", l.Requests, l.Period) }

// validate reports whether l can be enforced with microsecond precision.
func (l Limit) validate() error {
	if l.Requests < 1 {
		return fmt.Errorf("ratelimit: limit must allow at least 1 request, got %d", l.Requests)
	}
	if l.Period.Microseconds() < int64(l.Requests) {
		return fmt.Errorf("ratelimit: period %s is too short for %d requests (need >= 1µs per request)", l.Period, l.Requests)
	}
	return nil
}

// Algorithm selects the rate limiting algorithm.
type Algorithm int

const (
	// GCRA is the Generic Cell Rate Algorithm. It allows a burst of up to
	// Limit.Requests and then spaces requests evenly at Period/Requests.
	GCRA Algorithm = iota
	// SlidingWindow is a sliding-window counter. It allows up to
	// Limit.Requests in any window of length Period (approximately, by
	// weighting the previous fixed window).
	SlidingWindow
)

// String returns the short name used in Redis keys.
func (a Algorithm) String() string {
	switch a {
	case GCRA:
		return "gcra"
	case SlidingWindow:
		return "sw"
	default:
		return fmt.Sprintf("Algorithm(%d)", int(a))
	}
}

// ErrInvalidCost is returned when n is negative or greater than the limit,
// i.e. a request that could never be allowed.
var ErrInvalidCost = errors.New("ratelimit: cost must be between 0 and the limit")

// DefaultPrefix is the default prefix for Redis keys.
const DefaultPrefix = "ratelimit:"

// Option configures a limiter.
type Option func(*config)

type config struct {
	algorithm Algorithm
	prefix    string
	now       func() time.Time
}

func newConfig(opts []Option) config {
	c := config{algorithm: GCRA, prefix: DefaultPrefix}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithAlgorithm selects the algorithm. The default is GCRA.
func WithAlgorithm(a Algorithm) Option { return func(c *config) { c.algorithm = a } }

// WithPrefix sets the key prefix. The full key is prefix + algorithm + ":" +
// key, e.g. "ratelimit:gcra:user:42". The default is DefaultPrefix.
func WithPrefix(p string) Option { return func(c *config) { c.prefix = p } }

// WithClock sets the time source.
//
// The memory limiter defaults to time.Now. The Redis limiter defaults to the
// Redis server clock (the TIME command), which keeps all clients consistent
// regardless of their local clock skew; set a clock only for tests or when
// your hosts are tightly synchronised.
func WithClock(now func() time.Time) Option { return func(c *config) { c.now = now } }

func (c config) storageKey(key string) string {
	return c.prefix + c.algorithm.String() + ":" + key
}

func checkCost(n int, l Limit) error {
	if n < 0 || n > l.Requests {
		return fmt.Errorf("%w: got %d, limit %d", ErrInvalidCost, n, l.Requests)
	}
	return nil
}

func makeResult(r raw, l Limit) Result {
	return Result{
		Allowed:    r.allowed,
		Limit:      l.Requests,
		Remaining:  int(r.remaining),
		RetryAfter: time.Duration(r.retryAfter) * time.Microsecond,
		ResetAfter: time.Duration(r.resetAfter) * time.Microsecond,
	}
}

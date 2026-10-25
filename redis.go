package ratelimit

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// The Lua scripts live in the top-level scripts/ directory so that the Go
// module and the NestJS package share one source of truth.

//go:embed scripts/gcra.lua
var gcraLua string

//go:embed scripts/sliding_window.lua
var slidingWindowLua string

var (
	gcraScript          = redis.NewScript(gcraLua)
	slidingWindowScript = redis.NewScript(slidingWindowLua)
)

// RedisClient is the subset of the go-redis API used by RedisLimiter. It is
// implemented by *redis.Client, *redis.ClusterClient and *redis.Ring.
type RedisClient interface {
	redis.Scripter
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// RedisLimiter is a distributed Limiter backed by Redis. Each decision is a
// single atomic Lua script invocation (EVALSHA, falling back to EVAL), so it
// is safe across any number of processes sharing the same Redis.
type RedisLimiter struct {
	client RedisClient
	limit  Limit
	cfg    config
	script *redis.Script
}

var _ Limiter = (*RedisLimiter)(nil)

// NewRedis returns a Redis-backed limiter enforcing limit. client may be a
// *redis.Client, *redis.ClusterClient or *redis.Ring. It panics if limit is
// invalid (fewer than one request, or a period shorter than one microsecond
// per request).
func NewRedis(client RedisClient, limit Limit, opts ...Option) *RedisLimiter {
	if err := limit.validate(); err != nil {
		panic(err)
	}
	cfg := newConfig(opts)
	s := gcraScript
	if cfg.algorithm == SlidingWindow {
		s = slidingWindowScript
	}
	return &RedisLimiter{client: client, limit: limit, cfg: cfg, script: s}
}

// Limit returns the limit enforced by l.
func (l *RedisLimiter) Limit() Limit { return l.limit }

// Allow implements Limiter.
func (l *RedisLimiter) Allow(ctx context.Context, key string, n int) (Result, error) {
	if err := checkCost(n, l.limit); err != nil {
		return Result{}, err
	}
	var now int64 // 0 tells the script to use the Redis server clock.
	if l.cfg.now != nil {
		now = l.cfg.now().UnixMicro()
	}
	v, err := l.script.Run(ctx, l.client,
		[]string{l.cfg.storageKey(key)},
		l.limit.Requests, l.limit.Period.Microseconds(), n, now,
	).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: redis: %w", err)
	}
	if len(v) != 4 {
		return Result{}, fmt.Errorf("ratelimit: redis: unexpected script reply %v", v)
	}
	return makeResult(raw{allowed: v[0] == 1, remaining: v[1], retryAfter: v[2], resetAfter: v[3]}, l.limit), nil
}

// Reset forgets all state for key.
func (l *RedisLimiter) Reset(ctx context.Context, key string) error {
	return l.client.Del(ctx, l.cfg.storageKey(key)).Err()
}

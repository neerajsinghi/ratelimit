package ratelimit

import (
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock is a manually advanced clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	// Aligned to a whole second so sliding-window boundaries are predictable.
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = time.Unix(1_700_000_000, 0).Add(d)
}

// newRedisClient starts an in-process miniredis server for the test.
func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	m := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// backends returns a memory and a Redis limiter with the same configuration
// so that every behavioural test runs against both implementations.
func backends(t *testing.T, l Limit, opts ...Option) map[string]Limiter {
	t.Helper()
	return map[string]Limiter{
		"memory": NewMemory(l, opts...),
		"redis":  NewRedis(newRedisClient(t), l, opts...),
	}
}

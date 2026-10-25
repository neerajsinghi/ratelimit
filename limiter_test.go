package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type step struct {
	at         time.Duration // time since the start of the test
	cost       int
	allowed    bool
	remaining  int
	retryAfter time.Duration
	resetAfter time.Duration
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func runSteps(t *testing.T, algo Algorithm, l Limit, steps []step) {
	t.Helper()
	clock := newFakeClock()
	for name, lim := range backends(t, l, WithAlgorithm(algo), WithClock(clock.Now)) {
		t.Run(name, func(t *testing.T) {
			for i, s := range steps {
				clock.Set(s.at)
				got, err := lim.Allow(context.Background(), "k", s.cost)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				want := Result{Allowed: s.allowed, Limit: l.Requests, Remaining: s.remaining, RetryAfter: s.retryAfter, ResetAfter: s.resetAfter}
				if got != want {
					t.Fatalf("step %d at %v cost %d:\n got %+v\nwant %+v", i, s.at, s.cost, got, want)
				}
			}
		})
	}
}

func TestGCRA(t *testing.T) {
	// 10 req/s: emission interval 100ms, burst 10.
	burst := make([]step, 0, 11)
	for i := 1; i <= 10; i++ {
		burst = append(burst, step{at: 0, cost: 1, allowed: true, remaining: 10 - i, resetAfter: ms(100 * i)})
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "burst then deny",
			steps: append(burst,
				step{at: 0, cost: 1, allowed: false, remaining: 0, retryAfter: ms(100), resetAfter: ms(1000)},
			),
		},
		{
			name: "steady rate after burst",
			steps: append(burst,
				step{at: ms(100), cost: 1, allowed: true, remaining: 0, resetAfter: ms(1000)},
				step{at: ms(100), cost: 1, allowed: false, remaining: 0, retryAfter: ms(100), resetAfter: ms(1000)},
				step{at: ms(150), cost: 1, allowed: false, remaining: 0, retryAfter: ms(50), resetAfter: ms(950)},
				step{at: ms(200), cost: 1, allowed: true, remaining: 0, resetAfter: ms(1000)},
				// Half an emission interval of slack does not add capacity.
				step{at: ms(350), cost: 1, allowed: true, remaining: 0, resetAfter: ms(950)},
				step{at: ms(350), cost: 1, allowed: false, remaining: 0, retryAfter: ms(50), resetAfter: ms(950)},
			),
		},
		{
			name: "full recovery after idle",
			steps: append(burst,
				step{at: ms(3000), cost: 1, allowed: true, remaining: 9, resetAfter: ms(100)},
			),
		},
		{
			name: "weighted cost and peek",
			steps: []step{
				{at: 0, cost: 5, allowed: true, remaining: 5, resetAfter: ms(500)},
				{at: 0, cost: 0, allowed: true, remaining: 5, resetAfter: ms(500)},
				{at: 0, cost: 6, allowed: false, remaining: 5, retryAfter: ms(100), resetAfter: ms(500)},
				{at: 0, cost: 5, allowed: true, remaining: 0, resetAfter: ms(1000)},
				{at: ms(250), cost: 0, allowed: true, remaining: 2, resetAfter: ms(750)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runSteps(t, GCRA, PerSecond(10), tt.steps) })
	}
}

func TestSlidingWindow(t *testing.T) {
	// 10 requests per 1s sliding window; the fake clock starts on a window
	// boundary.
	full := make([]step, 0, 10)
	for i := 1; i <= 10; i++ {
		full = append(full, step{at: 0, cost: 1, allowed: true, remaining: 10 - i, resetAfter: ms(2000)})
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "fill window then deny until previous decays",
			steps: append(full,
				// Next window is needed; there 10 * (1 - e) + 1 <= 10 at e = 0.1.
				step{at: 0, cost: 1, allowed: false, remaining: 0, retryAfter: ms(1100), resetAfter: ms(2000)},
				step{at: ms(999), cost: 1, allowed: false, remaining: 0, retryAfter: ms(101), resetAfter: ms(1001)},
				step{at: ms(1100), cost: 1, allowed: true, remaining: 0, resetAfter: ms(1900)},
				// Estimate is now 10*0.9 + 1 = 10: wait for another 10% to decay.
				step{at: ms(1100), cost: 1, allowed: false, remaining: 0, retryAfter: ms(100), resetAfter: ms(1900)},
				step{at: ms(1200), cost: 1, allowed: true, remaining: 0, resetAfter: ms(1800)},
				// Two windows later only the 2 requests of window 1 count, half weighted.
				step{at: ms(2500), cost: 1, allowed: true, remaining: 8, resetAfter: ms(1500)},
			),
		},
		{
			name: "fresh after two idle windows",
			steps: append(full,
				step{at: ms(5000), cost: 1, allowed: true, remaining: 9, resetAfter: ms(2000)},
			),
		},
		{
			name: "weighted cost and peek",
			steps: []step{
				{at: 0, cost: 0, allowed: true, remaining: 10, resetAfter: 0},
				{at: ms(500), cost: 4, allowed: true, remaining: 6, resetAfter: ms(1500)},
				{at: ms(600), cost: 7, allowed: false, remaining: 6, retryAfter: ms(650), resetAfter: ms(1400)},
				{at: ms(1500), cost: 7, allowed: true, remaining: 1, resetAfter: ms(1500)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runSteps(t, SlidingWindow, PerSecond(10), tt.steps) })
	}
}

func TestSlidingWindowNeverExceedsLimitInAnyWindow(t *testing.T) {
	// Hammer the limiter every 7ms for 5s and check that no 1s window
	// contains more than the limit.
	clock := newFakeClock()
	for name, lim := range backends(t, PerSecond(20), WithAlgorithm(SlidingWindow), WithClock(clock.Now)) {
		t.Run(name, func(t *testing.T) {
			var allowedAt []time.Duration
			for at := time.Duration(0); at < 5*time.Second; at += 7 * time.Millisecond {
				clock.Set(at)
				res, err := lim.Allow(context.Background(), "k", 1)
				if err != nil {
					t.Fatal(err)
				}
				if res.Allowed {
					allowedAt = append(allowedAt, at)
				}
			}
			for i := range allowedAt {
				n := 0
				for j := i; j < len(allowedAt) && allowedAt[j]-allowedAt[i] < time.Second; j++ {
					n++
				}
				if n > 20 {
					t.Fatalf("%d requests allowed within 1s starting at %v", n, allowedAt[i])
				}
			}
			if len(allowedAt) < 90 {
				t.Fatalf("only %d requests allowed in 5s at 20/s", len(allowedAt))
			}
		})
	}
}

func TestKeysAreIndependent(t *testing.T) {
	for _, algo := range []Algorithm{GCRA, SlidingWindow} {
		for name, lim := range backends(t, PerMinute(1), WithAlgorithm(algo), WithClock(newFakeClock().Now)) {
			t.Run(algo.String()+"/"+name, func(t *testing.T) {
				ctx := context.Background()
				for _, k := range []string{"a", "b"} {
					if res, _ := lim.Allow(ctx, k, 1); !res.Allowed {
						t.Fatalf("first request for %q denied", k)
					}
				}
				if res, _ := lim.Allow(ctx, "a", 1); res.Allowed {
					t.Fatal("second request for a allowed")
				}
			})
		}
	}
}

func TestInvalidCost(t *testing.T) {
	for name, lim := range backends(t, PerSecond(5)) {
		for _, n := range []int{-1, 6} {
			if _, err := lim.Allow(context.Background(), "k", n); !errors.Is(err, ErrInvalidCost) {
				t.Errorf("%s: Allow(n=%d) err = %v, want ErrInvalidCost", name, n, err)
			}
		}
	}
}

func TestInvalidLimitPanics(t *testing.T) {
	for _, l := range []Limit{{0, time.Second}, {10, 5 * time.Microsecond}, {-1, time.Second}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewMemory(%v) did not panic", l)
				}
			}()
			NewMemory(l)
		}()
	}
}

func TestRedisServerClock(t *testing.T) {
	// Without WithClock the script reads the Redis TIME command.
	lim := NewRedis(newRedisClient(t), PerHour(2))
	ctx := context.Background()
	for i, want := range []bool{true, true, false} {
		res, err := lim.Allow(ctx, "k", 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Allowed != want {
			t.Fatalf("request %d: allowed = %v, want %v", i, res.Allowed, want)
		}
	}
	if err := lim.Reset(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if res, _ := lim.Allow(ctx, "k", 1); !res.Allowed {
		t.Fatal("request after Reset denied")
	}
}

func TestRedisKeyLayout(t *testing.T) {
	c := newRedisClient(t)
	ctx := context.Background()
	for algo, want := range map[Algorithm]string{GCRA: "app:gcra:user:1", SlidingWindow: "app:sw:user:1"} {
		lim := NewRedis(c, PerSecond(1), WithAlgorithm(algo), WithPrefix("app:"))
		if _, err := lim.Allow(ctx, "user:1", 1); err != nil {
			t.Fatal(err)
		}
		if n, _ := c.Exists(ctx, want).Result(); n != 1 {
			t.Errorf("%v: key %q not found", algo, want)
		}
		if ttl, _ := c.PTTL(ctx, want).Result(); ttl <= 0 {
			t.Errorf("%v: key %q has no TTL", algo, want)
		}
	}
}

func TestConcurrentAllow(t *testing.T) {
	const limit, workers, perWorker = 100, 20, 25
	for _, algo := range []Algorithm{GCRA, SlidingWindow} {
		for name, lim := range backends(t, PerHour(limit), WithAlgorithm(algo)) {
			t.Run(algo.String()+"/"+name, func(t *testing.T) {
				var allowed atomic.Int64
				var wg sync.WaitGroup
				for w := 0; w < workers; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; i < perWorker; i++ {
							res, err := lim.Allow(context.Background(), "shared", 1)
							if err != nil {
								t.Error(err)
								return
							}
							if res.Allowed {
								allowed.Add(1)
							}
						}
					}()
				}
				wg.Wait()
				if got := allowed.Load(); got != limit {
					t.Fatalf("allowed %d of %d requests, want exactly %d", got, workers*perWorker, limit)
				}
			})
		}
	}
}

func TestMemorySweepsExpiredKeys(t *testing.T) {
	clock := newFakeClock()
	lim := NewMemory(PerSecond(1), WithClock(clock.Now))
	for i := 0; i < sweepEvery-1; i++ {
		_, _ = lim.Allow(context.Background(), fmt.Sprintf("key-%d", i), 1)
	}
	clock.Set(time.Minute)
	_, _ = lim.Allow(context.Background(), "last", 1)
	lim.mu.Lock()
	n := len(lim.entries)
	lim.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d entries after sweep, want 1", n)
	}
}

func TestContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, lim := range backends(t, PerSecond(1)) {
		if _, err := lim.Allow(ctx, "k", 1); err == nil {
			t.Errorf("%s: no error for canceled context", name)
		}
	}
}

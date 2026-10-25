package ratelimit_test

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/neerajsinghi/ratelimit"
)

func ExampleNewMemory() {
	limiter := ratelimit.NewMemory(ratelimit.PerSecond(2))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, _ := limiter.Allow(ctx, "user:42", 1)
		fmt.Println(res.Allowed, res.Remaining)
	}
	// Output:
	// true 1
	// true 0
	// false 0
}

func ExampleMiddleware() {
	// In production use ratelimit.NewRedis(redisClient, ...) instead so that
	// every instance shares the same limits.
	limiter := ratelimit.NewMemory(ratelimit.PerMinute(100), ratelimit.WithAlgorithm(ratelimit.SlidingWindow))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "hello") })

	handler := ratelimit.Middleware(limiter,
		ratelimit.WithKeyFunc(ratelimit.KeyChain(ratelimit.KeyByHeader("X-API-Key"), ratelimit.KeyByIP())),
	)(mux)

	srv := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	_ = srv // srv.ListenAndServe()
}

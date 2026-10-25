package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
})

func TestMiddlewareHeaders(t *testing.T) {
	clock := newFakeClock()
	h := Middleware(NewMemory(PerMinute(2), WithClock(clock.Now)))(okHandler)

	tests := []struct {
		remote                  string
		status                  int
		limit, remaining, reset string
		retryAfter              string
	}{
		// 2/min: emission interval 30s.
		{"10.0.0.1:1111", 200, "2", "1", "30", ""},
		{"10.0.0.1:2222", 200, "2", "0", "60", ""},
		{"10.0.0.1:3333", 429, "2", "0", "60", "30"},
		{"10.0.0.2:1111", 200, "2", "1", "30", ""},
	}
	for i, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tt.remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != tt.status {
			t.Errorf("request %d: status %d, want %d", i, rec.Code, tt.status)
		}
		for name, want := range map[string]string{
			HeaderLimit:      tt.limit,
			HeaderRemaining:  tt.remaining,
			HeaderReset:      tt.reset,
			HeaderRetryAfter: tt.retryAfter,
		} {
			if got := rec.Header().Get(name); got != want {
				t.Errorf("request %d: %s = %q, want %q", i, name, got, want)
			}
		}
	}
}

func TestMiddlewareCustomDeniedHandlerAndCost(t *testing.T) {
	denied := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := Middleware(NewMemory(PerMinute(4)), WithCost(3), WithDeniedHandler(denied))(okHandler)
	codes := []int{http.StatusOK, http.StatusTeapot}
	for i, want := range codes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != want {
			t.Fatalf("request %d: status %d, want %d", i, rec.Code, want)
		}
		if i == 1 && rec.Header().Get(HeaderRetryAfter) == "" {
			t.Fatal("Retry-After not set for custom denied handler")
		}
	}
}

type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string, int) (Result, error) {
	return Result{}, errors.New("redis down")
}

func TestMiddlewareErrors(t *testing.T) {
	tests := []struct {
		name   string
		lim    Limiter
		opts   []MiddlewareOption
		header string
		status int
	}{
		{"missing key", NewMemory(PerSecond(1)), []MiddlewareOption{WithKeyFunc(KeyByHeader("X-API-Key"))}, "", 400},
		{"key present", NewMemory(PerSecond(1)), []MiddlewareOption{WithKeyFunc(KeyByHeader("X-API-Key"))}, "abc", 200},
		{"limiter error fails closed", failingLimiter{}, nil, "", 503},
		{"limiter error fails open", failingLimiter{}, []MiddlewareOption{WithFailOpen(true)}, "", 200},
		{"custom error handler", failingLimiter{}, []MiddlewareOption{WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusInternalServerError)
		})}, "", 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("X-API-Key", tt.header)
			}
			rec := httptest.NewRecorder()
			Middleware(tt.lim, tt.opts...)(okHandler).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d", rec.Code, tt.status)
			}
		})
	}
}

func TestKeyFuncs(t *testing.T) {
	tests := []struct {
		name    string
		fn      KeyFunc
		remote  string
		headers map[string]string
		want    string
		wantErr error
	}{
		{"ip v4", KeyByIP(), "1.2.3.4:5678", nil, "ip:1.2.3.4", nil},
		{"ip v6", KeyByIP(), "[::1]:5678", nil, "ip:::1", nil},
		{"ip without port", KeyByIP(), "1.2.3.4", nil, "ip:1.2.3.4", nil},
		{"ip empty", KeyByIP(), "", nil, "", ErrNoKey},
		{"header", KeyByHeader("x-api-key"), "", map[string]string{"X-Api-Key": "k1"}, "h:X-Api-Key:k1", nil},
		{"header first of list", KeyByHeader("X-Forwarded-For"), "", map[string]string{"X-Forwarded-For": " 9.9.9.9 , 10.0.0.1"}, "h:X-Forwarded-For:9.9.9.9", nil},
		{"header missing", KeyByHeader("X-Api-Key"), "", nil, "", ErrNoKey},
		{"chain falls back", KeyChain(KeyByHeader("X-Api-Key"), KeyByIP()), "1.2.3.4:1", nil, "ip:1.2.3.4", nil},
		{"chain prefers first", KeyChain(KeyByHeader("X-Api-Key"), KeyByIP()), "1.2.3.4:1", map[string]string{"X-Api-Key": "k"}, "h:X-Api-Key:k", nil},
		{"chain exhausted", KeyChain(KeyByHeader("A"), KeyByHeader("B")), "", nil, "", ErrNoKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			got, err := tt.fn(req)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestSetHeadersRoundsUp(t *testing.T) {
	h := http.Header{}
	SetHeaders(h, Result{Limit: 5, Remaining: 0, RetryAfter: 1500 * time.Millisecond, ResetAfter: time.Millisecond})
	if h.Get(HeaderRetryAfter) != "2" || h.Get(HeaderReset) != "1" {
		t.Fatalf("headers = %v", h)
	}
	h = http.Header{}
	SetHeaders(h, Result{Limit: 5, RetryAfter: time.Microsecond})
	if h.Get(HeaderRetryAfter) != "1" {
		t.Fatalf("Retry-After = %q, want 1", h.Get(HeaderRetryAfter))
	}
}

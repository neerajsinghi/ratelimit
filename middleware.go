package ratelimit

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names set by Middleware, following the IETF "RateLimit header
// fields for HTTP" draft plus the standard Retry-After header.
const (
	HeaderLimit      = "RateLimit-Limit"
	HeaderRemaining  = "RateLimit-Remaining"
	HeaderReset      = "RateLimit-Reset"
	HeaderRetryAfter = "Retry-After"
)

// ErrNoKey is returned by a KeyFunc that cannot derive a key from a request.
var ErrNoKey = errors.New("ratelimit: no rate limit key for request")

// KeyFunc derives the rate limit key for a request.
type KeyFunc func(r *http.Request) (string, error)

// KeyByIP keys requests by the IP address of the direct peer
// (http.Request.RemoteAddr). Behind a reverse proxy, use KeyByHeader with the
// header your proxy sets (for example "X-Real-IP") instead.
func KeyByIP() KeyFunc {
	return func(r *http.Request) (string, error) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if host == "" {
			return "", ErrNoKey
		}
		return "ip:" + host, nil
	}
}

// KeyByHeader keys requests by the value of the named header, such as an API
// key or a proxy-provided client IP. For comma-separated headers such as
// X-Forwarded-For the first element is used. It returns ErrNoKey if the
// header is absent or empty.
func KeyByHeader(name string) KeyFunc {
	canonical := http.CanonicalHeaderKey(name)
	return func(r *http.Request) (string, error) {
		v := r.Header.Get(canonical)
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return "", ErrNoKey
		}
		return "h:" + canonical + ":" + v, nil
	}
}

// KeyChain tries each KeyFunc in order and returns the first key found,
// e.g. KeyChain(KeyByHeader("X-API-Key"), KeyByIP()).
func KeyChain(fns ...KeyFunc) KeyFunc {
	return func(r *http.Request) (string, error) {
		for _, fn := range fns {
			k, err := fn(r)
			if err == nil {
				return k, nil
			}
			if !errors.Is(err, ErrNoKey) {
				return "", err
			}
		}
		return "", ErrNoKey
	}
}

// MiddlewareOption configures Middleware.
type MiddlewareOption func(*middleware)

type middleware struct {
	limiter  Limiter
	keyFunc  KeyFunc
	cost     int
	failOpen bool
	onDenied http.Handler
	onError  func(w http.ResponseWriter, r *http.Request, err error)
}

// WithKeyFunc sets how requests are keyed. The default is KeyByIP().
func WithKeyFunc(fn KeyFunc) MiddlewareOption { return func(m *middleware) { m.keyFunc = fn } }

// WithCost sets how many units each request consumes. The default is 1.
func WithCost(n int) MiddlewareOption { return func(m *middleware) { m.cost = n } }

// WithFailOpen lets requests through when the limiter returns an error
// (for example when Redis is unreachable). By default such requests get
// 503 Service Unavailable.
func WithFailOpen(open bool) MiddlewareOption { return func(m *middleware) { m.failOpen = open } }

// WithDeniedHandler sets the handler that serves rate limited requests. The
// rate limit headers (including Retry-After) are already set when it runs;
// the handler writes the status and body. The default writes 429 Too Many
// Requests with a short text body.
func WithDeniedHandler(h http.Handler) MiddlewareOption {
	return func(m *middleware) { m.onDenied = h }
}

// WithErrorHandler sets the handler for key derivation and limiter errors.
// The default responds 400 Bad Request for ErrNoKey and 503 Service
// Unavailable otherwise.
func WithErrorHandler(fn func(w http.ResponseWriter, r *http.Request, err error)) MiddlewareOption {
	return func(m *middleware) { m.onError = fn }
}

// Middleware returns net/http middleware that enforces l on every request.
//
// Every limited response carries RateLimit-Limit, RateLimit-Remaining and
// RateLimit-Reset (seconds). Denied requests additionally get Retry-After
// (seconds) and, by default, status 429 Too Many Requests.
func Middleware(l Limiter, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	m := &middleware{
		limiter: l,
		keyFunc: KeyByIP(),
		cost:    1,
		onDenied: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		}),
		onError: defaultErrorHandler,
	}
	for _, o := range opts {
		o(m)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, err := m.keyFunc(r)
			if err != nil {
				m.onError(w, r, err)
				return
			}
			res, err := m.limiter.Allow(r.Context(), key, m.cost)
			if err != nil {
				if m.failOpen {
					next.ServeHTTP(w, r)
					return
				}
				m.onError(w, r, err)
				return
			}
			SetHeaders(w.Header(), res)
			if !res.Allowed {
				m.onDenied.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func defaultErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, ErrNoKey) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

// SetHeaders writes the RateLimit-* headers for res to h, plus Retry-After
// when res is not allowed. It is exported for use by other frameworks.
func SetHeaders(h http.Header, res Result) {
	h.Set(HeaderLimit, strconv.Itoa(res.Limit))
	h.Set(HeaderRemaining, strconv.Itoa(res.Remaining))
	h.Set(HeaderReset, strconv.FormatInt(ceilSeconds(res.ResetAfter), 10))
	if !res.Allowed {
		retry := ceilSeconds(res.RetryAfter)
		if retry < 1 {
			retry = 1
		}
		h.Set(HeaderRetryAfter, strconv.FormatInt(retry, 10))
	}
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}

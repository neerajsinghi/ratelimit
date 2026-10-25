import { HEADER_LIMIT, HEADER_REMAINING, HEADER_RESET, HEADER_RETRY_AFTER } from './constants';
import type { RateLimitResult } from './limiter';

const ceilSeconds = (ms: number): number => (ms > 0 ? Math.ceil(ms / 1000) : 0);

/**
 * Computes the rate limit response headers for a result: RateLimit-Limit,
 * RateLimit-Remaining and RateLimit-Reset (seconds), plus Retry-After
 * (seconds, at least 1) when the request is denied. Identical to the Go
 * module's SetHeaders.
 */
export function rateLimitHeaders(result: RateLimitResult): Record<string, string> {
  const headers: Record<string, string> = {
    [HEADER_LIMIT]: String(result.limit),
    [HEADER_REMAINING]: String(result.remaining),
    [HEADER_RESET]: String(ceilSeconds(result.resetAfterMs)),
  };
  if (!result.allowed) {
    headers[HEADER_RETRY_AFTER] = String(Math.max(1, ceilSeconds(result.retryAfterMs)));
  }
  return headers;
}

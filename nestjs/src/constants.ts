/** Injection token for the module options. */
export const RATE_LIMIT_OPTIONS = Symbol('RATE_LIMIT_OPTIONS');
/** Injection token for the RateLimiter instance used by the guard. */
export const RATE_LIMITER = Symbol('RATE_LIMITER');
/** Metadata key set by the @RateLimit() decorator. */
export const RATE_LIMIT_METADATA = 'ratelimit:options';
/** Metadata key set by the @SkipRateLimit() decorator. */
export const SKIP_RATE_LIMIT_METADATA = 'ratelimit:skip';

/** Header names, following the IETF RateLimit header fields draft. */
export const HEADER_LIMIT = 'RateLimit-Limit';
export const HEADER_REMAINING = 'RateLimit-Remaining';
export const HEADER_RESET = 'RateLimit-Reset';
export const HEADER_RETRY_AFTER = 'Retry-After';

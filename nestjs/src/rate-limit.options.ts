import type { Cluster, Redis } from 'ioredis';
import type { Algorithm, LimitSpec, RateLimiter } from './limiter';

/** Options for RateLimitModule.forRoot(). */
export interface RateLimitModuleOptions {
  /**
   * ioredis client (Redis or Cluster) for distributed limits. When neither
   * `redis` nor `limiter` is set, an in-memory limiter is used.
   */
  redis?: Redis | Cluster;
  /** A custom limiter; takes precedence over `redis`. */
  limiter?: RateLimiter;
  /** Algorithm for the built-in limiters. Default `gcra`. */
  algorithm?: Algorithm;
  /** Redis key prefix. Default `ratelimit:`. */
  prefix?: string;
  /**
   * Derives the client identifier from the request. Returning undefined or ''
   * responds 400 Bad Request. Default: `ip:<request.ip>`.
   */
  keyGenerator?: (request: any) => string | undefined | Promise<string | undefined>;
  /** Limit applied to routes without @RateLimit(). Default: none. */
  defaultLimit?: LimitSpec;
  /**
   * Let requests through when the limiter fails (e.g. Redis is down).
   * Default false: such requests get 503 Service Unavailable.
   */
  failOpen?: boolean;
  /**
   * Register RateLimitGuard as a global guard. It only acts on routes with
   * @RateLimit() (or all routes when `defaultLimit` is set). Default true.
   */
  globalGuard?: boolean;
}

/** Options for RateLimitModule.forRootAsync(). */
export interface RateLimitModuleAsyncOptions {
  imports?: any[];
  inject?: any[];
  useFactory: (...args: any[]) => RateLimitModuleOptions | Promise<RateLimitModuleOptions>;
  /** See RateLimitModuleOptions.globalGuard. Must be known statically. */
  globalGuard?: boolean;
}

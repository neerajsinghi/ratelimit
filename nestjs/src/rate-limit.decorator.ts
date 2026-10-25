import { SetMetadata } from '@nestjs/common';
import { RATE_LIMIT_METADATA, SKIP_RATE_LIMIT_METADATA } from './constants';
import type { LimitSpec } from './limiter';

/** Options for the @RateLimit() decorator. */
export interface RateLimitOptions extends LimitSpec {
  /** Units consumed per request. Default 1. */
  cost?: number;
  /**
   * Bucket name. Routes with the same name share one bucket per client.
   * Default `<ControllerName>.<handlerName>`.
   */
  name?: string;
  /** Derives the client identifier; overrides the module's keyGenerator. */
  key?: (request: any) => string | undefined | Promise<string | undefined>;
}

/**
 * Rate limits a controller or route handler. A handler-level decorator
 * overrides a controller-level one.
 *
 * @example
 * ```ts
 * @RateLimit({ limit: 100, period: 60_000 }) // 100 requests per minute
 * @Get()
 * findAll() {}
 * ```
 */
export const RateLimit = (options: RateLimitOptions): MethodDecorator & ClassDecorator =>
  SetMetadata(RATE_LIMIT_METADATA, options);

/** Exempts a controller or route handler from rate limiting. */
export const SkipRateLimit = (): MethodDecorator & ClassDecorator =>
  SetMetadata(SKIP_RATE_LIMIT_METADATA, true);

import { DynamicModule, Module, Provider } from '@nestjs/common';
import { APP_GUARD } from '@nestjs/core';
import { RATE_LIMITER, RATE_LIMIT_OPTIONS } from './constants';
import { MemoryRateLimiter, RateLimiter, RedisRateLimiter } from './limiter';
import { RateLimitGuard } from './rate-limit.guard';
import type { RateLimitModuleAsyncOptions, RateLimitModuleOptions } from './rate-limit.options';

/** Builds the limiter described by the module options. */
export function createLimiter(options: RateLimitModuleOptions): RateLimiter {
  if (options.limiter) {
    return options.limiter;
  }
  if (options.redis) {
    return new RedisRateLimiter(options.redis, {
      algorithm: options.algorithm,
      prefix: options.prefix,
    });
  }
  return new MemoryRateLimiter({ algorithm: options.algorithm });
}

const limiterProvider: Provider = {
  provide: RATE_LIMITER,
  useFactory: createLimiter,
  inject: [RATE_LIMIT_OPTIONS],
};

/**
 * Distributed rate limiting for NestJS.
 *
 * @example
 * ```ts
 * @Module({
 *   imports: [RateLimitModule.forRoot({ redis: new Redis() })],
 * })
 * export class AppModule {}
 * ```
 */
@Module({})
export class RateLimitModule {
  static forRoot(options: RateLimitModuleOptions = {}): DynamicModule {
    return RateLimitModule.build(
      [{ provide: RATE_LIMIT_OPTIONS, useValue: options }],
      [],
      options.globalGuard ?? true,
    );
  }

  static forRootAsync(options: RateLimitModuleAsyncOptions): DynamicModule {
    return RateLimitModule.build(
      [{ provide: RATE_LIMIT_OPTIONS, useFactory: options.useFactory, inject: options.inject ?? [] }],
      options.imports ?? [],
      options.globalGuard ?? true,
    );
  }

  private static build(optionProviders: Provider[], imports: any[], globalGuard: boolean): DynamicModule {
    const providers: Provider[] = [...optionProviders, limiterProvider, RateLimitGuard];
    if (globalGuard) {
      providers.push({ provide: APP_GUARD, useExisting: RateLimitGuard });
    }
    return {
      module: RateLimitModule,
      global: true,
      imports,
      providers,
      exports: [RATE_LIMITER, RATE_LIMIT_OPTIONS, RateLimitGuard],
    };
  }
}

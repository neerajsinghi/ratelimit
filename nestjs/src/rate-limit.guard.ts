import {
  BadRequestException,
  CanActivate,
  ExecutionContext,
  HttpException,
  HttpStatus,
  Inject,
  Injectable,
  ServiceUnavailableException,
} from '@nestjs/common';
import { Reflector } from '@nestjs/core';
import {
  RATE_LIMITER,
  RATE_LIMIT_METADATA,
  RATE_LIMIT_OPTIONS,
  SKIP_RATE_LIMIT_METADATA,
} from './constants';
import { rateLimitHeaders } from './headers';
import type { RateLimiter, RateLimitResult } from './limiter';
import type { RateLimitOptions } from './rate-limit.decorator';
import type { RateLimitModuleOptions } from './rate-limit.options';

/** Default client identifier: the request IP, like the Go KeyByIP(). */
export function defaultKeyGenerator(request: any): string | undefined {
  const ip: string | undefined = request?.ip ?? request?.socket?.remoteAddress;
  return ip ? `ip:${ip}` : undefined;
}

/**
 * Enforces @RateLimit() on HTTP routes, sets the RateLimit-* headers and
 * responds 429 Too Many Requests (with Retry-After) when the limit is hit.
 */
@Injectable()
export class RateLimitGuard implements CanActivate {
  constructor(
    private readonly reflector: Reflector,
    @Inject(RATE_LIMITER) private readonly limiter: RateLimiter,
    @Inject(RATE_LIMIT_OPTIONS) private readonly options: RateLimitModuleOptions,
  ) {}

  async canActivate(context: ExecutionContext): Promise<boolean> {
    if (context.getType() !== 'http') {
      return true;
    }
    const targets = [context.getHandler(), context.getClass()];
    if (this.reflector.getAllAndOverride<boolean>(SKIP_RATE_LIMIT_METADATA, targets)) {
      return true;
    }
    const route = this.reflector.getAllAndOverride<RateLimitOptions | undefined>(
      RATE_LIMIT_METADATA,
      targets,
    );
    const spec: RateLimitOptions | undefined = route ?? this.options.defaultLimit;
    if (!spec) {
      return true;
    }

    const http = context.switchToHttp();
    const request = http.getRequest();
    const response = http.getResponse();

    const keyFn = spec.key ?? this.options.keyGenerator ?? defaultKeyGenerator;
    const id = await keyFn(request);
    if (!id) {
      throw new BadRequestException('Unable to identify client for rate limiting');
    }
    const scope = spec.name ?? `${context.getClass().name}.${context.getHandler().name}`;

    let result: RateLimitResult;
    try {
      result = await this.limiter.allow(`${scope}:${id}`, spec, spec.cost ?? 1);
    } catch (err) {
      if (err instanceof RangeError) {
        throw err; // misconfiguration, not an outage
      }
      if (this.options.failOpen) {
        return true;
      }
      throw new ServiceUnavailableException();
    }

    for (const [name, value] of Object.entries(rateLimitHeaders(result))) {
      setHeader(response, name, value);
    }
    if (!result.allowed) {
      throw new HttpException('Too Many Requests', HttpStatus.TOO_MANY_REQUESTS);
    }
    return true;
  }
}

/** Sets a header on an Express or Fastify response. */
function setHeader(response: any, name: string, value: string): void {
  if (typeof response?.header === 'function') {
    response.header(name, value);
  } else if (typeof response?.setHeader === 'function') {
    response.setHeader(name, value);
  }
}

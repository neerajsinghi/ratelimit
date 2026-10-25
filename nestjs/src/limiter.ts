import type { Cluster, Redis } from 'ioredis';
import {
  gcraStep,
  GcraState,
  RawResult,
  slidingWindowStep,
  SlidingWindowState,
} from './algorithms';
import { GCRA_LUA, SLIDING_WINDOW_LUA } from './lua.generated';

/**
 * Rate limiting algorithm.
 *
 * - `gcra`: Generic Cell Rate Algorithm. Allows a burst of up to `limit`, then
 *   spaces requests evenly at `period / limit`.
 * - `sliding-window`: sliding-window counter. Allows up to `limit` requests in
 *   any window of length `period` (approximated by weighting the previous fixed
 *   window).
 */
export type Algorithm = 'gcra' | 'sliding-window';

/** A rate of `limit` requests per `period` milliseconds. */
export interface LimitSpec {
  /** Requests allowed per period (also the burst size for GCRA). */
  limit: number;
  /** Period length in milliseconds. */
  period: number;
}

/** The outcome of a rate limit check. */
export interface RateLimitResult {
  /** Whether the request may proceed. */
  allowed: boolean;
  /** Requests allowed per period. */
  limit: number;
  /** How many more units could be consumed immediately. */
  remaining: number;
  /** Milliseconds until the same request would be allowed (0 if allowed). */
  retryAfterMs: number;
  /** Milliseconds until the key is back to its full, untouched state. */
  resetAfterMs: number;
}

/** Decides whether an action identified by a key may proceed. */
export interface RateLimiter {
  /**
   * Consumes `cost` units (default 1) for `key` if allowed. Nothing is
   * consumed when the result is not allowed; `cost = 0` only inspects.
   */
  allow(key: string, spec: LimitSpec, cost?: number): Promise<RateLimitResult>;
}

export interface LimiterOptions {
  /** Algorithm to use. Default `gcra`. */
  algorithm?: Algorithm;
  /**
   * Key prefix. The stored key is `prefix + algorithm + ":" + key`, e.g.
   * `ratelimit:gcra:user:42` — identical to the Go module, so services in
   * both languages can share limits. Default `ratelimit:`.
   */
  prefix?: string;
  /**
   * Time source in epoch milliseconds (fractions allowed). The memory limiter
   * defaults to `Date.now`; the Redis limiter defaults to the Redis server
   * clock (TIME), which keeps all clients consistent. Set it only for tests.
   */
  clock?: () => number;
}

export const DEFAULT_PREFIX = 'ratelimit:';

const KEY_INFIX: Record<Algorithm, string> = { gcra: 'gcra', 'sliding-window': 'sw' };

interface Validated {
  limit: number;
  periodUs: number;
  cost: number;
}

/** Validates inputs, throwing RangeError for impossible configurations. */
function validate(spec: LimitSpec, cost: number): Validated {
  const { limit } = spec;
  const periodUs = Math.round(spec.period * 1000);
  if (!Number.isInteger(limit) || limit < 1) {
    throw new RangeError(`ratelimit: limit must be an integer >= 1, got ${limit}`);
  }
  if (!(periodUs >= limit)) {
    throw new RangeError(
      `ratelimit: period ${spec.period}ms is too short for ${limit} requests (need >= 1µs per request)`,
    );
  }
  if (!Number.isInteger(cost) || cost < 0 || cost > limit) {
    throw new RangeError(`ratelimit: cost must be an integer between 0 and ${limit}, got ${cost}`);
  }
  return { limit, periodUs, cost };
}

function toResult(r: RawResult, limit: number): RateLimitResult {
  return {
    allowed: r.allowed,
    limit,
    remaining: r.remaining,
    retryAfterMs: r.retryAfterUs / 1000,
    resetAfterMs: r.resetAfterUs / 1000,
  };
}

interface MemoryEntry {
  gcra?: GcraState;
  sw?: SlidingWindowState;
  expiresAtUs: number;
}

const SWEEP_EVERY = 1024;

/**
 * In-process limiter implementing exactly the same algorithms as
 * RedisRateLimiter. State is local to one process: use it for tests,
 * development, or single-instance services.
 */
export class MemoryRateLimiter implements RateLimiter {
  private readonly entries = new Map<string, MemoryEntry>();
  private readonly algorithm: Algorithm;
  private readonly clock: () => number;
  private calls = 0;

  constructor(options: Omit<LimiterOptions, 'prefix'> = {}) {
    this.algorithm = options.algorithm ?? 'gcra';
    this.clock = options.clock ?? Date.now;
  }

  async allow(key: string, spec: LimitSpec, cost = 1): Promise<RateLimitResult> {
    const { limit, periodUs } = validate(spec, cost);
    const now = Math.round(this.clock() * 1000);
    const k = key;

    if (++this.calls % SWEEP_EVERY === 0) {
      this.sweep(now);
    }

    let entry = this.entries.get(k);
    if (entry && entry.expiresAtUs <= now) {
      this.entries.delete(k);
      entry = undefined;
    }

    if (this.algorithm === 'sliding-window') {
      const step = slidingWindowStep(limit, periodUs, cost, now, entry?.sw);
      if (step.state) {
        this.entries.set(k, { sw: step.state, expiresAtUs: now + step.ttlUs });
      }
      return toResult(step.result, limit);
    }
    const step = gcraStep(limit, periodUs, cost, now, entry?.gcra);
    if (step.state) {
      this.entries.set(k, { gcra: step.state, expiresAtUs: now + step.ttlUs });
    }
    return toResult(step.result, limit);
  }

  /** Forgets all state. */
  clear(): void {
    this.entries.clear();
  }

  /** Number of tracked keys (for tests and metrics). */
  get size(): number {
    return this.entries.size;
  }

  private sweep(now: number): void {
    for (const [k, e] of this.entries) {
      if (e.expiresAtUs <= now) {
        this.entries.delete(k);
      }
    }
  }
}

type RedisLike = Redis | Cluster;

type ScriptCommand = (key: string, ...args: (string | number)[]) => Promise<unknown>;

const COMMANDS: Record<Algorithm, { name: string; lua: string }> = {
  gcra: { name: 'ratelimitGcra', lua: GCRA_LUA },
  'sliding-window': { name: 'ratelimitSlidingWindow', lua: SLIDING_WINDOW_LUA },
};

/**
 * Distributed limiter backed by Redis (ioredis `Redis` or `Cluster`). Each
 * decision is one atomic Lua script call (EVALSHA with EVAL fallback), using
 * the same scripts as the Go module.
 */
export class RedisRateLimiter implements RateLimiter {
  private readonly algorithm: Algorithm;
  private readonly prefix: string;
  private readonly clock?: () => number;
  private readonly command: ScriptCommand;

  constructor(redis: RedisLike, options: LimiterOptions = {}) {
    this.algorithm = options.algorithm ?? 'gcra';
    this.prefix = options.prefix ?? DEFAULT_PREFIX;
    this.clock = options.clock;

    const { name, lua } = COMMANDS[this.algorithm];
    const client = redis as unknown as Record<string, ScriptCommand | undefined>;
    if (typeof client[name] !== 'function') {
      redis.defineCommand(name, { numberOfKeys: 1, lua });
    }
    this.command = (client[name] as ScriptCommand).bind(redis);
  }

  /** The Redis key used for `key`. */
  storageKey(key: string): string {
    return `${this.prefix}${KEY_INFIX[this.algorithm]}:${key}`;
  }

  async allow(key: string, spec: LimitSpec, cost = 1): Promise<RateLimitResult> {
    const { limit, periodUs } = validate(spec, cost);
    // 0 tells the script to use the Redis server clock.
    const now = this.clock ? Math.round(this.clock() * 1000) : 0;
    const reply = await this.command(this.storageKey(key), limit, periodUs, cost, now);
    if (!Array.isArray(reply) || reply.length !== 4) {
      throw new Error(`ratelimit: unexpected script reply ${JSON.stringify(reply)}`);
    }
    // Redis converts Lua numbers to integers by truncation; do the same in
    // case a Lua 5.3 emulator returns floats.
    const [allowed, remaining, retryAfterUs, resetAfterUs] = reply.map((v) => Math.trunc(Number(v)));
    return toResult({ allowed: allowed === 1, remaining, retryAfterUs, resetAfterUs }, limit);
  }
}

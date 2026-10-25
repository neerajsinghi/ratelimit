import RedisMock from 'ioredis-mock';
import { MemoryRateLimiter, rateLimitHeaders, RedisRateLimiter } from '../src';

const T0 = 1_700_000_000_000; // ms, aligned to a whole second

describe('MemoryRateLimiter', () => {
  it('GCRA allows a burst then spaces requests evenly', async () => {
    let now = T0;
    const l = new MemoryRateLimiter({ clock: () => now });
    const spec = { limit: 10, period: 1000 }; // emission interval 100ms
    for (let i = 1; i <= 10; i++) {
      await expect(l.allow('k', spec)).resolves.toEqual({
        allowed: true,
        limit: 10,
        remaining: 10 - i,
        retryAfterMs: 0,
        resetAfterMs: 100 * i,
      });
    }
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: false, retryAfterMs: 100 });
    now += 100;
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: true, remaining: 0 });
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: false });
  });

  it('sliding window weights the previous window', async () => {
    let now = T0;
    const l = new MemoryRateLimiter({ algorithm: 'sliding-window', clock: () => now });
    const spec = { limit: 10, period: 1000 };
    for (let i = 0; i < 10; i++) await l.allow('k', spec);
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: false, retryAfterMs: 1100 });
    now = T0 + 1100;
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: true, remaining: 0 });
    await expect(l.allow('k', spec)).resolves.toMatchObject({ allowed: false, retryAfterMs: 100 });
  });

  it('peeks with cost 0 and keeps keys independent', async () => {
    const l = new MemoryRateLimiter();
    const spec = { limit: 1, period: 60_000 };
    await expect(l.allow('a', spec, 0)).resolves.toMatchObject({ allowed: true, remaining: 1 });
    await expect(l.allow('a', spec)).resolves.toMatchObject({ allowed: true });
    await expect(l.allow('a', spec)).resolves.toMatchObject({ allowed: false });
    await expect(l.allow('b', spec)).resolves.toMatchObject({ allowed: true });
  });

  it.each([
    [{ limit: 0, period: 1000 }, 1],
    [{ limit: 1.5, period: 1000 }, 1],
    [{ limit: 10, period: 0.005 }, 1],
    [{ limit: 5, period: 1000 }, 6],
    [{ limit: 5, period: 1000 }, -1],
  ])('rejects invalid input %j cost %d', async (spec, cost) => {
    await expect(new MemoryRateLimiter().allow('k', spec, cost)).rejects.toThrow(RangeError);
  });

  it('sweeps expired keys', async () => {
    let now = T0;
    const l = new MemoryRateLimiter({ clock: () => now });
    for (let i = 0; i < 1023; i++) await l.allow(`k${i}`, { limit: 1, period: 1000 });
    expect(l.size).toBe(1023);
    now += 60_000;
    await l.allow('last', { limit: 1, period: 1000 });
    expect(l.size).toBe(1);
  });
});

describe('RedisRateLimiter', () => {
  const redis = new RedisMock();
  beforeEach(() => redis.flushall());
  afterAll(() => redis.disconnect());

  it('uses the same key layout as the Go module', async () => {
    const gcra = new RedisRateLimiter(redis, { prefix: 'app:' });
    const sw = new RedisRateLimiter(redis, { prefix: 'app:', algorithm: 'sliding-window' });
    await gcra.allow('user:1', { limit: 1, period: 1000 });
    await sw.allow('user:1', { limit: 1, period: 1000 });
    expect(gcra.storageKey('user:1')).toBe('app:gcra:user:1');
    expect(await redis.exists('app:gcra:user:1', 'app:sw:user:1')).toBe(2);
    expect(await redis.pttl('app:gcra:user:1')).toBeGreaterThan(0);
  });

  it('defaults to the Redis server clock', async () => {
    const l = new RedisRateLimiter(redis);
    const spec = { limit: 2, period: 3_600_000 };
    const allowed = [];
    for (let i = 0; i < 3; i++) allowed.push((await l.allow('k', spec)).allowed);
    expect(allowed).toEqual([true, true, false]);
  });

  it('can share one client between limiters', async () => {
    const a = new RedisRateLimiter(redis);
    const b = new RedisRateLimiter(redis);
    await a.allow('shared', { limit: 1, period: 60_000 });
    await expect(b.allow('shared', { limit: 1, period: 60_000 })).resolves.toMatchObject({ allowed: false });
  });
});

describe('rateLimitHeaders', () => {
  it('rounds seconds up and sets Retry-After only when denied', () => {
    expect(
      rateLimitHeaders({ allowed: true, limit: 5, remaining: 4, retryAfterMs: 0, resetAfterMs: 1 }),
    ).toEqual({ 'RateLimit-Limit': '5', 'RateLimit-Remaining': '4', 'RateLimit-Reset': '1' });
    expect(
      rateLimitHeaders({ allowed: false, limit: 5, remaining: 0, retryAfterMs: 0.001, resetAfterMs: 1500 }),
    ).toEqual({
      'RateLimit-Limit': '5',
      'RateLimit-Remaining': '0',
      'RateLimit-Reset': '2',
      'Retry-After': '1',
    });
  });
});

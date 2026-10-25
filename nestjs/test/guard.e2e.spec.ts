import 'reflect-metadata';
import { Controller, Get, INestApplication, Injectable, Module } from '@nestjs/common';
import { Test } from '@nestjs/testing';
import RedisMock from 'ioredis-mock';
import request from 'supertest';
import {
  RATE_LIMITER,
  RateLimit,
  RateLimiter,
  RateLimitModule,
  RateLimitModuleOptions,
  RateLimitResult,
  SkipRateLimit,
} from '../src';

@Controller()
class AppController {
  @RateLimit({ limit: 2, period: 60_000 })
  @Get('limited')
  limited() {
    return 'ok';
  }

  @Get('open')
  open() {
    return 'ok';
  }

  @RateLimit({ limit: 4, period: 60_000, cost: 3 })
  @Get('expensive')
  expensive() {
    return 'ok';
  }

  @RateLimit({ limit: 1, period: 60_000, key: (req) => req.headers['x-api-key'] })
  @Get('by-key')
  byKey() {
    return 'ok';
  }

  @RateLimit({ limit: 1, period: 60_000, name: 'shared' })
  @Get('shared-a')
  sharedA() {
    return 'ok';
  }

  @RateLimit({ limit: 1, period: 60_000, name: 'shared' })
  @Get('shared-b')
  sharedB() {
    return 'ok';
  }
}

@RateLimit({ limit: 1, period: 60_000 })
@Controller('class')
class ClassLimitedController {
  @Get('a')
  a() {
    return 'ok';
  }

  @SkipRateLimit()
  @Get('skipped')
  skipped() {
    return 'ok';
  }

  @RateLimit({ limit: 3, period: 60_000 })
  @Get('override')
  override() {
    return 'ok';
  }
}

async function createApp(options: RateLimitModuleOptions): Promise<INestApplication> {
  const moduleRef = await Test.createTestingModule({
    imports: [RateLimitModule.forRoot(options)],
    controllers: [AppController, ClassLimitedController],
  }).compile();
  const app = moduleRef.createNestApplication();
  await app.init();
  return app;
}

describe('RateLimitGuard', () => {
  let app: INestApplication;
  const redis = new RedisMock();

  beforeEach(async () => {
    await redis.flushall();
    app = await createApp({ redis });
  });
  afterEach(() => app.close());
  afterAll(() => redis.disconnect());

  it('sets RateLimit headers and returns 429 with Retry-After', async () => {
    const server = app.getHttpServer();
    // 2 per minute with GCRA: emission interval 30s.
    const r1 = await request(server).get('/limited').expect(200);
    expect(r1.headers['ratelimit-limit']).toBe('2');
    expect(r1.headers['ratelimit-remaining']).toBe('1');
    expect(r1.headers['ratelimit-reset']).toBe('30');
    expect(r1.headers['retry-after']).toBeUndefined();

    const r2 = await request(server).get('/limited').expect(200);
    expect(r2.headers['ratelimit-remaining']).toBe('0');
    expect(r2.headers['ratelimit-reset']).toBe('60');

    const r3 = await request(server).get('/limited').expect(429);
    expect(r3.headers['ratelimit-remaining']).toBe('0');
    expect(['29', '30']).toContain(r3.headers['retry-after']);
    expect(r3.body).toMatchObject({ statusCode: 429 });
  });

  it('ignores routes without @RateLimit()', async () => {
    const r = await request(app.getHttpServer()).get('/open').expect(200);
    expect(r.headers['ratelimit-limit']).toBeUndefined();
  });

  it('applies cost', async () => {
    const server = app.getHttpServer();
    await request(server).get('/expensive').expect(200);
    await request(server).get('/expensive').expect(429);
  });

  it('uses a custom key and rejects unidentified clients', async () => {
    const server = app.getHttpServer();
    await request(server).get('/by-key').set('x-api-key', 'a').expect(200);
    await request(server).get('/by-key').set('x-api-key', 'a').expect(429);
    await request(server).get('/by-key').set('x-api-key', 'b').expect(200);
    await request(server).get('/by-key').expect(400);
  });

  it('shares buckets by name', async () => {
    const server = app.getHttpServer();
    await request(server).get('/shared-a').expect(200);
    await request(server).get('/shared-b').expect(429);
  });

  it('supports controller-level limits, overrides and skipping', async () => {
    const server = app.getHttpServer();
    await request(server).get('/class/a').expect(200);
    await request(server).get('/class/a').expect(429);
    for (let i = 0; i < 3; i++) await request(server).get('/class/override').expect(200);
    await request(server).get('/class/override').expect(429);
    for (let i = 0; i < 3; i++) await request(server).get('/class/skipped').expect(200);
  });
});

describe('RateLimitModule options', () => {
  const failing: RateLimiter = {
    allow: async () => {
      throw new Error('redis down');
    },
  };

  it('fails closed with 503 by default', async () => {
    const app = await createApp({ limiter: failing });
    await request(app.getHttpServer()).get('/limited').expect(503);
    await app.close();
  });

  it('fails open when configured', async () => {
    const app = await createApp({ limiter: failing, failOpen: true });
    await request(app.getHttpServer()).get('/limited').expect(200);
    await app.close();
  });

  it('applies defaultLimit and a custom keyGenerator, with the memory limiter', async () => {
    const app = await createApp({
      defaultLimit: { limit: 1, period: 60_000 },
      keyGenerator: (req) => req.headers['x-user'],
    });
    const server = app.getHttpServer();
    await request(server).get('/open').set('x-user', 'u1').expect(200);
    await request(server).get('/open').set('x-user', 'u1').expect(429);
    await request(server).get('/open').set('x-user', 'u2').expect(200);
    await app.close();
  });

  it('can register the guard manually', async () => {
    const app = await createApp({ globalGuard: false });
    for (let i = 0; i < 3; i++) await request(app.getHttpServer()).get('/limited').expect(200);
    await app.close();
  });

  it('supports forRootAsync and exposes the limiter', async () => {
    const calls: string[] = [];
    @Injectable()
    class Recorder implements RateLimiter {
      async allow(key: string): Promise<RateLimitResult> {
        calls.push(key);
        return { allowed: true, limit: 1, remaining: 0, retryAfterMs: 0, resetAfterMs: 0 };
      }
    }
    @Module({ providers: [Recorder], exports: [Recorder] })
    class RecorderModule {}

    const moduleRef = await Test.createTestingModule({
      imports: [
        RateLimitModule.forRootAsync({
          imports: [RecorderModule],
          inject: [Recorder],
          useFactory: (limiter: Recorder) => ({ limiter }),
        }),
      ],
      controllers: [AppController],
    }).compile();
    const app = moduleRef.createNestApplication();
    await app.init();
    expect(app.get(RATE_LIMITER)).toBeInstanceOf(Recorder);
    await request(app.getHttpServer()).get('/limited').expect(200);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatch(/^AppController\.limited:ip:/);
    await app.close();
  });
});

# @neerajsinghi/nest-ratelimit

Distributed rate limiting for NestJS on Redis (ioredis), using GCRA or a sliding-window counter. The Lua scripts and semantics are identical to the Go module [`github.com/neerajsinghi/ratelimit`](https://github.com/neerajsinghi/ratelimit), so Go and Node.js services can share limits.

```ts
@Module({ imports: [RateLimitModule.forRoot({ redis: new Redis() })] })
export class AppModule {}

@Controller('orders')
export class OrdersController {
  @RateLimit({ limit: 100, period: 60_000 })
  @Get()
  list() {}
}
```

Responses carry `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset`. Denied requests get `429 Too Many Requests` with `Retry-After`.

See the [main README](https://github.com/neerajsinghi/ratelimit#readme) for the full documentation.

## Development

`npm test` and `npm run build` first run `tools/embed-lua.mjs`, which embeds `../scripts/*.lua` into `src/lua.generated.ts` (git-ignored). Edit the scripts in the top-level `scripts/` directory, never the generated file.

MIT © 2026 Neeraj Singhi

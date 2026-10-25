-- GCRA (Generic Cell Rate Algorithm) rate limiter.
--
-- This file is the single source of truth for the Redis implementation and is
-- shared verbatim by the Go module (go:embed) and the NestJS package (embedded
-- at build time). The in-memory implementations in both languages mirror the
-- arithmetic below line for line, so all four backends return identical
-- results for identical inputs.
--
-- All times are integer microseconds. Every intermediate value is an integer
-- below 2^53, so the double arithmetic of Lua 5.1 is exact.
--
-- KEYS[1]  bucket key; stores the theoretical arrival time (TAT)
-- ARGV[1]  limit   - requests allowed per period (also the burst size)
-- ARGV[2]  period  - period length in microseconds (must be >= limit)
-- ARGV[3]  cost    - tokens to consume (0 peeks without consuming)
-- ARGV[4]  now     - current time in microseconds; <= 0 uses Redis TIME
--
-- Returns {allowed (0|1), remaining, retry_after_us, reset_after_us}.

-- Force every value to a float. Redis runs Lua 5.1, where all numbers are
-- doubles already; Lua 5.3 based emulators (such as ioredis-mock) would
-- otherwise use integer arithmetic, which may be 32-bit and overflow.
local function num(v)
  local n = tonumber(v)
  if n == nil then
    return nil
  end
  return n + 0.0
end
local function floor(x) return math.floor(x) + 0.0 end
local function ceil(x) return math.ceil(x) + 0.0 end

local key = KEYS[1]
local limit = num(ARGV[1])
local period = num(ARGV[2])
local cost = num(ARGV[3])
local now = num(ARGV[4])

if now <= 0 then
  if redis.replicate_commands then
    redis.replicate_commands()
  end
  local t = redis.call('TIME')
  now = num(t[1]) * 1000000 + num(t[2])
end

local interval = floor(period / limit)
local burst = interval * limit

local tat = num(redis.call('GET', key))
if tat == nil or tat < now then
  tat = now
end

local new_tat = tat + interval * cost
local diff = now - (new_tat - burst)

if diff < 0 then
  local remaining = floor((now - (tat - burst)) / interval)
  if remaining < 0 then
    remaining = 0
  end
  return {0, remaining, -diff, tat - now}
end

local reset_after = new_tat - now
if cost > 0 then
  redis.call('SET', key, string.format('%.0f', new_tat),
    'PX', string.format('%.0f', math.max(1, ceil(reset_after / 1000))))
end
return {1, floor(diff / interval), 0, reset_after}

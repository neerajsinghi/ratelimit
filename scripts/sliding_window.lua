-- Sliding-window counter rate limiter.
--
-- This file is the single source of truth for the Redis implementation and is
-- shared verbatim by the Go module (go:embed) and the NestJS package (embedded
-- at build time). The in-memory implementations in both languages mirror the
-- arithmetic below line for line, so all four backends return identical
-- results for identical inputs.
--
-- Time is cut into fixed windows of length `period`. The request rate is
-- estimated as the count of the current window plus the count of the previous
-- window weighted by how much of it still overlaps the sliding window:
--
--   estimate = previous * (period - elapsed) / period + current
--
-- State is a single hash {w = window index, c = current count, p = previous
-- count}, so memory is O(1) per key regardless of the limit.
--
-- KEYS[1]  bucket key (hash)
-- ARGV[1]  limit   - requests allowed per sliding window
-- ARGV[2]  period  - window length in microseconds
-- ARGV[3]  cost    - requests to record (0 peeks without recording)
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

local window = floor(now / period)
local elapsed = now - window * period

local state = redis.call('HMGET', key, 'w', 'c', 'p')
local w = num(state[1])
local c = 0
local p = 0
if w == window then
  c = num(state[2]) or 0
  p = num(state[3]) or 0
elseif w == window - 1 then
  p = num(state[2]) or 0
end

local estimate = p * (period - elapsed) / period + c

local function reset_after(cur, prev)
  if cur > 0 then
    return period - elapsed + period
  elseif prev > 0 then
    return period - elapsed
  end
  return 0
end

if estimate + cost <= limit then
  local remaining = floor(limit - estimate - cost)
  c = c + cost
  if cost > 0 then
    redis.call('HSET', key,
      'w', string.format('%.0f', window),
      'c', string.format('%.0f', c),
      'p', string.format('%.0f', p))
    redis.call('PEXPIRE', key,
      string.format('%.0f', ceil((period - elapsed + period) / 1000)))
  end
  return {1, remaining, 0, reset_after(c, p)}
end

local remaining = floor(limit - estimate)
if remaining < 0 then
  remaining = 0
end

local retry_after
if c + cost <= limit then
  -- The previous window decays enough within the current window.
  retry_after = ceil(period - (limit - c - cost) * period / p) - elapsed
else
  -- Wait for the next window, where the current count becomes the previous.
  retry_after = period - elapsed
  local extra = ceil(period - (limit - cost) * period / c)
  if extra > 0 then
    retry_after = retry_after + extra
  end
end

return {0, remaining, retry_after, reset_after(c, p)}

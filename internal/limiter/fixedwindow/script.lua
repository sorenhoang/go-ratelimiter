-- Fixed window counter.
--
-- One EVAL decides and records; nothing can interleave between the read and the
-- increment. The clock comes from the Redis server so that several app servers
-- agree on where a window starts.
--
--   KEYS[1]  base key, without the window suffix
--   ARGV[1]  window length, milliseconds
--   ARGV[2]  limit per window
--   ARGV[3]  cost of this request
--
--   returns  { allowed, remaining, reset_after_ms, retry_after_ms }
--            allowed is 1 or 0 -- a Lua boolean would reach the client as nil

local window = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])

-- TIME yields { seconds, microseconds } as strings.
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local window_start = now - (now % window)
local key = KEYS[1] .. ':' .. window_start

-- Always in [1, window], so it is never a zero TTL, and because it is measured
-- from the fixed window_start it shrinks on every call instead of sliding.
local reset_after = window - (now % window)

local count = tonumber(redis.call('GET', key) or '0')

if count + cost > limit then
  -- A limit lowered at runtime can leave count above it; do not report negative.
  local remaining = limit - count
  if remaining < 0 then
    remaining = 0
  end
  return { 0, remaining, reset_after, reset_after }
end

count = redis.call('INCRBY', key, cost)
redis.call('PEXPIRE', key, reset_after)

return { 1, limit - count, reset_after, 0 }

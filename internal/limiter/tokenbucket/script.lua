-- Token bucket.
--
-- A bucket holds up to `capacity` tokens and refills at `rate` per second. A
-- request spends `cost` of them, and is refused when the bucket is short. A
-- caller who has been quiet arrives to a full bucket and may spend all of it at
-- once: unlike every other limiter here, the burst is the point.
--
-- The refill is computed from elapsed time when the bucket is read, so there is
-- no ticker to run and nothing to supervise.
--
--   KEYS[1]  the key
--   ARGV[1]  capacity, whole tokens
--   ARGV[2]  refill rate, tokens per second, may be fractional
--   ARGV[3]  cost of this request
--
--   returns  { allowed, remaining, reset_after_ms, retry_after_ms }

local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'refilled_at')
local tokens = tonumber(bucket[1])
local refilled_at = tonumber(bucket[2])

-- A key that has never been seen is a full bucket. Reading a missing hash as
-- zero would refuse every caller's first ever request.
if tokens == nil or refilled_at == nil then
  tokens = capacity
  refilled_at = now
end

local elapsed = now - refilled_at
if elapsed > 0 then
  tokens = math.min(capacity, tokens + elapsed / 1000 * rate)
  refilled_at = now
end

local allowed = tokens >= cost
local retry_after = 0
if allowed then
  tokens = tokens - cost
else
  -- Exact, not estimated: the deficit divided by the rate is precisely how long
  -- it takes for this request to fit.
  retry_after = math.ceil((cost - tokens) / rate * 1000)
end

local reset_after = math.ceil((capacity - tokens) / rate * 1000)

-- tostring keeps the fraction. Redis preserves decimals in a command argument,
-- but truncates a Lua number returned to the client, so the partial token has to
-- live in the hash and only whole ones are reported back.
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'refilled_at', refilled_at)

-- Idle for longer than a full refill takes and the bucket would be brim-full
-- anyway, so dropping the key gives the same answer for free.
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / rate * 1000))

local allowed_flag = 0
if allowed then
  allowed_flag = 1
end

return { allowed_flag, math.floor(tokens), reset_after, retry_after }

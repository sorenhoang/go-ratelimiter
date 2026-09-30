-- Leaky bucket, metered.
--
-- Water pours in at a request's cost and drains at a constant rate. A request
-- that would overflow the bucket is refused.
--
-- This is the exact dual of the token bucket in phase 04: level = capacity -
-- tokens, and every comparison maps across. Counting water in rather than
-- tokens left changes nothing about which requests pass -- it changes what the
-- numbers mean when you read them, and which one you find easier to reason
-- about when the bucket is nearly full.
--
--   KEYS[1]  the key
--   ARGV[1]  capacity, whole units
--   ARGV[2]  leak rate, units per second, may be fractional
--   ARGV[3]  cost of this request
--
--   returns  { allowed, remaining, reset_after_ms, retry_after_ms }

local capacity = tonumber(ARGV[1])
local leak = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local bucket = redis.call('HMGET', KEYS[1], 'level', 'leaked_at')
local level = tonumber(bucket[1])
local leaked_at = tonumber(bucket[2])

-- A key never seen is an empty bucket. Note this is the mirror of phase 04,
-- where a missing key meant a *full* one: empty here and full there are the
-- same state seen from opposite ends.
if level == nil or leaked_at == nil then
  level = 0
  leaked_at = now
end

local elapsed = now - leaked_at
if elapsed > 0 then
  level = math.max(0, level - elapsed / 1000 * leak)
  leaked_at = now
end

local allowed = (level + cost) <= capacity
local retry_after = 0
if allowed then
  level = level + cost
else
  -- Exact: how much has to drain for this request to fit, over the rate.
  retry_after = math.ceil((level + cost - capacity) / leak * 1000)
end

local reset_after = math.ceil(level / leak * 1000)

-- tostring keeps the fraction. Redis preserves decimals in a command argument
-- but truncates a Lua number returned to the client, so the partial unit lives
-- in the hash and only whole ones are reported back.
redis.call('HSET', KEYS[1], 'level', tostring(level), 'leaked_at', leaked_at)

-- Idle for longer than a full drain and the bucket is empty anyway.
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / leak * 1000))

local remaining = math.floor(capacity - level)
if remaining < 0 then
  remaining = 0
end

local allowed_flag = 0
if allowed then
  allowed_flag = 1
end

return { allowed_flag, remaining, reset_after, retry_after }

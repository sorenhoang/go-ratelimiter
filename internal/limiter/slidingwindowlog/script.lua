-- Sliding window log.
--
-- Every request is recorded as one sorted set entry scored by its arrival time.
-- A decision is then just "how many entries still fall inside the window", which
-- makes this exact rather than approximate -- and costs O(limit) memory per key.
--
--   KEYS[1]  the key
--   ARGV[1]  window length, milliseconds
--   ARGV[2]  limit per window
--   ARGV[3]  cost of this request
--   ARGV[4]  a token unique to this request, supplied by the caller
--
--   returns  { allowed, remaining, reset_after_ms, retry_after_ms }

local window = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local token = ARGV[4]

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local key = KEYS[1]

-- Everything older than the window has slid out and no longer counts.
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)

local count = redis.call('ZCARD', key)

if count + cost > limit then
  -- A limit lowered at runtime can leave count above it; do not report negative.
  local remaining = limit - count
  if remaining < 0 then
    remaining = 0
  end

  -- Admitting `cost` more needs this many of the oldest entries to expire, so
  -- the wait is until that particular entry leaves the window. This algorithm
  -- knows the answer exactly; the counter variants in later phases can only
  -- estimate it.
  local need = count + cost - limit
  local nth = redis.call('ZRANGE', key, need - 1, need - 1, 'WITHSCORES')
  local retry_after = window
  if nth[2] then
    retry_after = math.floor(tonumber(nth[2]) + window - now)
  end

  -- The quota is whole again only once the newest entry leaves.
  local newest = redis.call('ZRANGE', key, -1, -1, 'WITHSCORES')
  local reset_after = 0
  if newest[2] then
    reset_after = math.floor(tonumber(newest[2]) + window - now)
  end

  return { 0, remaining, reset_after, retry_after }
end

-- One member per unit of cost, each distinct. Scoring by time alone would let
-- two requests in the same millisecond overwrite one another, the count would
-- run low, and traffic would leak past the limit.
for i = 1, cost do
  redis.call('ZADD', key, now, token .. ':' .. i)
end

-- Refreshed on every accepted request, so a caller that goes quiet for a whole
-- window stops costing memory.
redis.call('PEXPIRE', key, window)

return { 1, limit - (count + cost), window, 0 }

-- Sliding window counter.
--
-- Two integers instead of one entry per request: the count for the current
-- fixed window, and the count for the one before it weighted by how much of it
-- is still in view. That makes this O(1) in memory, and an estimate rather than
-- an exact answer.
--
-- The weighting assumes the previous window's requests were spread evenly
-- across it. Traffic that arrived in a burst breaks that assumption, which is
-- the error this algorithm trades accuracy for memory to get.
--
--   KEYS[1]  base key, without the window suffix
--   ARGV[1]  window length, milliseconds
--   ARGV[2]  limit per window
--   ARGV[3]  cost of this request
--
--   returns  { allowed, remaining, reset_after_ms, retry_after_ms }

local window = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local window_start = now - (now % window)
local elapsed = now - window_start

local curr_key = KEYS[1] .. ':' .. window_start
local prev_key = KEYS[1] .. ':' .. (window_start - window)

local curr = tonumber(redis.call('GET', curr_key) or '0')
local prev = tonumber(redis.call('GET', prev_key) or '0')

-- How much of the previous window is still inside the trailing window.
local weight = (window - elapsed) / window
local estimate = prev * weight + curr

-- The fraction has to survive this comparison. Flooring the estimate first
-- would admit a request the limit does not have room for.
local allowed = (estimate + cost) <= limit

-- The estimate can only reach zero once the current window's own count has also
-- aged out, one window after this one ends.
local reset_after = 0
if curr > 0 then
  reset_after = 2 * window - elapsed
elseif prev > 0 then
  reset_after = window - elapsed
end

if not allowed then
  local remaining = math.floor(limit - estimate)
  if remaining < 0 then
    remaining = 0
  end

  -- An approximation, unlike the log in phase 02, which reads the answer off
  -- the entry that has to expire. Here the estimate falls as `weight` shrinks,
  -- so solve for the moment it drops far enough -- and fall back to the start of
  -- the next window when the current count alone already fills the limit.
  local retry_after = window - elapsed
  local headroom = limit - cost - curr
  if headroom > 0 and prev > 0 then
    local target_elapsed = window * (1 - headroom / prev)
    local wait = target_elapsed - elapsed
    if wait > 0 and wait < retry_after then
      retry_after = wait
    end
  end

  return { 0, remaining, math.floor(reset_after), math.ceil(retry_after) }
end

redis.call('INCRBY', curr_key, cost)
-- Twice the window, because the previous counter has to outlive its own window
-- or the next one has nothing to weight and this degrades into a fixed window.
redis.call('PEXPIRE', curr_key, 2 * window)

local remaining = math.floor(limit - estimate - cost)
if remaining < 0 then
  remaining = 0
end

return { 1, remaining, math.floor(2 * window - elapsed), 0 }

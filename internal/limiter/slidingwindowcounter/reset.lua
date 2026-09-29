-- Drop both counters this algorithm can read: the current window's and the
-- previous one's. Clearing only the current one would leave the previous count
-- still pulling the estimate up.
--
--   KEYS[1]  base key, without the window suffix
--   ARGV[1]  window length, milliseconds

local window = tonumber(ARGV[1])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local window_start = now - (now % window)

return redis.call('DEL',
  KEYS[1] .. ':' .. window_start,
  KEYS[1] .. ':' .. (window_start - window))

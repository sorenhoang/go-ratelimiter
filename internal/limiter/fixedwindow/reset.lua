-- Delete the counter for the window that is current right now.
--
--   KEYS[1]  base key, without the window suffix
--   ARGV[1]  window length, milliseconds
--
-- Only the current window is read by script.lua, so dropping it is a full reset.

local window = tonumber(ARGV[1])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)

local window_start = now - (now % window)

return redis.call('DEL', KEYS[1] .. ':' .. window_start)

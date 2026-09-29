-- Drop the whole log for one key.
--
-- This needs a script at all only because the limiter holds a redis.Scripter,
-- which exposes EVAL and nothing else -- deliberately, so the package cannot
-- reach for arbitrary commands behind the algorithm's back.
--
--   KEYS[1]  the key

return redis.call('DEL', KEYS[1])

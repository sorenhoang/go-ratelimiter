-- Drop the bucket, so the next request meets a full one.
--
--   KEYS[1]  the key

return redis.call('DEL', KEYS[1])

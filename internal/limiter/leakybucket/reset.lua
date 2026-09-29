-- Empty the bucket.
--
--   KEYS[1]  the key

return redis.call('DEL', KEYS[1])

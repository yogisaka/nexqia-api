-- KEYS[1] = window key
-- ARGV[1] = max_attempts
-- ARGV[2] = window_seconds
-- Returns {allowed (0/1), retry_after_seconds}
local key = KEYS[1]
local max_attempts = tonumber(ARGV[1])
local window_seconds = tonumber(ARGV[2])

local time_result = redis.call("TIME")
local now = tonumber(time_result[1]) + (tonumber(time_result[2]) / 1000000)
local window_start = now - window_seconds

redis.call("ZREMRANGEBYSCORE", key, "-inf", window_start)

local count = redis.call("ZCARD", key)

local allowed = 0
local retry_after = 0
if count < max_attempts then
  redis.call("ZADD", key, now, tostring(now) .. "-" .. tostring(math.random()))
  redis.call("EXPIRE", key, window_seconds)
  allowed = 1
else
  local oldest = redis.call("ZRANGE", key, 0, 0, "WITHSCORES")
  local oldest_score = tonumber(oldest[2])
  retry_after = math.ceil(oldest_score + window_seconds - now)
end

return {allowed, retry_after}

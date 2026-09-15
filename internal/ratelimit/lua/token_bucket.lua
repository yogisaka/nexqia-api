-- KEYS[1] = bucket key
-- ARGV[1] = capacity (max burst size)
-- ARGV[2] = refill_per_minute
-- Returns {allowed (0/1), retry_after_seconds}
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill_per_minute = tonumber(ARGV[2])
local refill_per_second = refill_per_minute / 60.0

local time_result = redis.call("TIME")
local now = tonumber(time_result[1]) + (tonumber(time_result[2]) / 1000000)

local bucket = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(bucket[1])
local last_refill = tonumber(bucket[2])

if tokens == nil then
  tokens = capacity
  last_refill = now
end

local elapsed = now - last_refill
if elapsed > 0 then
  tokens = math.min(capacity, tokens + elapsed * refill_per_second)
  last_refill = now
end

local allowed = 0
local retry_after = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  local deficit = 1 - tokens
  retry_after = math.ceil(deficit / refill_per_second)
end

redis.call("HMSET", key, "tokens", tostring(tokens), "last_refill", tostring(last_refill))
redis.call("EXPIRE", key, 3600)

return {allowed, retry_after}

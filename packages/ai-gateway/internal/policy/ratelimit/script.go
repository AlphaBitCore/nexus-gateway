// Package ratelimit provides distributed and local sliding window rate limiting.
package ratelimit

// SlidingWindowLua is an atomic sliding window rate limiter executed as a
// single EVALSHA call. Returns 0 if allowed, or retryAfterMs (>0) if blocked.
const SlidingWindowLua = `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local windowMs = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - windowMs)
local count = redis.call('ZCARD', key)
if count >= limit then
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    if #oldest >= 2 then
        local retryMs = tonumber(oldest[2]) + windowMs - now
        return retryMs
    end
    return 1
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, windowMs)
return 0
`

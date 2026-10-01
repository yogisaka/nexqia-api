// internal/ratelimit/limiter.go
package ratelimit

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

//go:embed lua/sliding_window.lua
var slidingWindowScript string

var errUnexpectedScriptResult = errors.New("ratelimit: unexpected script result shape")

// Limiter runs the atomic token-bucket/sliding-window Lua scripts against Redis
// via EVALSHA (redis.Script.Run handles the SCRIPT LOAD/EVALSHA/NOSCRIPT-fallback
// dance internally — see 2026-09-15-ratelimit-hardening-design.md §4).
type Limiter struct {
	client        *redis.Client
	tokenBucket   *redis.Script
	slidingWindow *redis.Script
}

func NewLimiter(client *redis.Client) *Limiter {
	return &Limiter{
		client:        client,
		tokenBucket:   redis.NewScript(tokenBucketScript),
		slidingWindow: redis.NewScript(slidingWindowScript),
	}
}

// Result is the outcome of a single rate-limit check.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration
}

// AllowTokenBucket checks/consumes one token from the named bucket. capacity is
// the max burst size, refillPerMinute is the steady-state refill rate.
func (l *Limiter) AllowTokenBucket(ctx context.Context, key string, capacity, refillPerMinute int) (Result, error) {
	return l.run(ctx, l.tokenBucket, key, capacity, refillPerMinute)
}

// AllowSlidingWindow checks/records one attempt against a sliding window of maxAttempts
// within windowSeconds.
func (l *Limiter) AllowSlidingWindow(ctx context.Context, key string, maxAttempts, windowSeconds int) (Result, error) {
	return l.run(ctx, l.slidingWindow, key, maxAttempts, windowSeconds)
}

func (l *Limiter) run(ctx context.Context, script *redis.Script, key string, arg1, arg2 int) (Result, error) {
	raw, err := script.Run(ctx, l.client, []string{key}, arg1, arg2).Result()
	if err != nil {
		return Result{}, err
	}
	values, ok := raw.([]interface{})
	if !ok || len(values) != 2 {
		return Result{}, errUnexpectedScriptResult
	}
	allowed, ok := values[0].(int64)
	if !ok {
		return Result{}, errUnexpectedScriptResult
	}
	retryAfterSeconds, ok := values[1].(int64)
	if !ok {
		return Result{}, errUnexpectedScriptResult
	}
	return Result{
		Allowed:    allowed == 1,
		RetryAfter: time.Duration(retryAfterSeconds) * time.Second,
	}, nil
}

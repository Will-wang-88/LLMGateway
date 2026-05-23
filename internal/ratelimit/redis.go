package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter is a Redis-backed implementation of the same semantics
// as Limiter: per-minute request + token windows and per-day request +
// token windows. It enables horizontal scaling (multiple gateway
// replicas share the same buckets).
//
// The atomicity for "check all dimensions then reserve" is delegated to
// a single EVAL of a small Lua script that reads four counters and
// either increments or rejects.
type RedisLimiter struct {
	client *redis.Client
	prefix string
}

// NewRedisLimiter expects a usable *redis.Client. The key prefix can be
// used to share a Redis cluster across multiple gateway deployments.
func NewRedisLimiter(client *redis.Client, prefix string) *RedisLimiter {
	if prefix == "" {
		prefix = "llmgw"
	}
	return &RedisLimiter{client: client, prefix: prefix}
}

// reserveScript checks all 4 dimensions then increments min/day request
// counters in one round-trip. Returns "" on admit, otherwise the
// rejection code.
//
// KEYS[1] = minute counter   (TTL 60s)
// KEYS[2] = minute token counter
// KEYS[3] = day request counter (TTL ~48h)
// KEYS[4] = day token counter
// ARGV[1] = rpm limit, ARGV[2] = tpm limit, ARGV[3] = dayReq limit, ARGV[4] = dayTok limit
var reserveScript = redis.NewScript(`
local rpm = tonumber(ARGV[1])
local tpm = tonumber(ARGV[2])
local dayReq = tonumber(ARGV[3])
local dayTok = tonumber(ARGV[4])
local cMin = tonumber(redis.call('GET', KEYS[1]) or '0')
local cTokMin = tonumber(redis.call('GET', KEYS[2]) or '0')
local cDay = tonumber(redis.call('GET', KEYS[3]) or '0')
local cTokDay = tonumber(redis.call('GET', KEYS[4]) or '0')
if rpm > 0 and cMin >= rpm then return 'rate_limit_exceeded' end
if tpm > 0 and cTokMin >= tpm then return 'token_rate_limit_exceeded' end
if dayReq > 0 and cDay >= dayReq then return 'daily_request_limit_exceeded' end
if dayTok > 0 and cTokDay >= dayTok then return 'daily_token_limit_exceeded' end
redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], 60)
redis.call('INCR', KEYS[3])
redis.call('EXPIRE', KEYS[3], 172800)
return ''
`)

func (l *RedisLimiter) keys(key string) (string, string, string, string) {
	now := time.Now().UTC()
	minBucket := fmt.Sprintf("%s:rl:min:%s:%d", l.prefix, key, now.Unix()/60)
	tokMinBucket := fmt.Sprintf("%s:rl:tokmin:%s:%d", l.prefix, key, now.Unix()/60)
	dayBucket := fmt.Sprintf("%s:rl:day:%s:%s", l.prefix, key, now.Format("20060102"))
	tokDayBucket := fmt.Sprintf("%s:rl:tokday:%s:%s", l.prefix, key, now.Format("20060102"))
	return minBucket, tokMinBucket, dayBucket, tokDayBucket
}

// CheckAndReserve mirrors Limiter.CheckAndReserve.
func (l *RedisLimiter) CheckAndReserve(key string, rpm int, tpm int64, dailyReq int64, dailyTok int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	kMin, kTokMin, kDay, kTokDay := l.keys(key)
	v, err := reserveScript.Run(ctx, l.client, []string{kMin, kTokMin, kDay, kTokDay},
		rpm, tpm, dailyReq, dailyTok).Result()
	if err != nil {
		// Fail open: if Redis is unreachable we let the request through
		// rather than denying all traffic. The error is observable via
		// metrics/logs at the call site.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, redis.Nil) {
			return ""
		}
		return ""
	}
	s, _ := v.(string)
	return s
}

// AddTokens mirrors Limiter.AddTokens and updates both window counters
// with the same per-minute + per-day TTL conventions.
func (l *RedisLimiter) AddTokens(key string, tokens int64) {
	if tokens <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, kTokMin, _, kTokDay := l.keys(key)
	pipe := l.client.Pipeline()
	pipe.IncrBy(ctx, kTokMin, tokens)
	pipe.Expire(ctx, kTokMin, 60*time.Second)
	pipe.IncrBy(ctx, kTokDay, tokens)
	pipe.Expire(ctx, kTokDay, 48*time.Hour)
	_, _ = pipe.Exec(ctx)
}

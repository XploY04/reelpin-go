// Package ratelimit is the token bucket in front of the expensive paths. The
// decision is made inside Redis in one atomic script, so two API replicas
// cannot each hand out the last token.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable means Redis could not answer. The caller decides whether that
// closes the door (paid or destructive work) or opens it (cheap reads).
var ErrUnavailable = errors.New("rate limiter unavailable")

// Policy is a refill rate plus a burst. Requests per Window refill the bucket
// steadily; Burst is how many may arrive at once.
type Policy struct {
	Name     string
	Requests int
	Window   time.Duration
	Burst    int
}

func (p Policy) capacity() int {
	if p.Burst > 0 {
		return p.Burst
	}
	return p.Requests
}

type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// bucketScript is a token bucket kept as two fields with one expiry. It returns
// whether the call is allowed, the tokens left, and how long until one refills.
//
// KEYS[1] bucket   ARGV[1] capacity  ARGV[2] refill per second
// ARGV[3] now (ms) ARGV[4] cost      ARGV[5] ttl (ms)
var bucketScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local refill_per_second = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local ttl_ms = tonumber(ARGV[5])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_ms')
local tokens = tonumber(state[1])
local updated_ms = tonumber(state[2])

if tokens == nil or updated_ms == nil then
  tokens = capacity
  updated_ms = now_ms
end

local elapsed_ms = math.max(0, now_ms - updated_ms)
tokens = math.min(capacity, tokens + (elapsed_ms / 1000.0) * refill_per_second)

local allowed = 0
local retry_after_ms = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
else
  local missing = cost - tokens
  retry_after_ms = math.ceil((missing / refill_per_second) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated_ms', now_ms)
redis.call('PEXPIRE', KEYS[1], ttl_ms)

return {allowed, math.floor(tokens), retry_after_ms}
`)

type Limiter struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

func New(client *redis.Client, prefix string) *Limiter {
	if prefix == "" {
		prefix = "reelpin:ratelimit"
	}
	return &Limiter{client: client, prefix: prefix, now: time.Now}
}

// WithClock is for tests that need to move time without sleeping.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	return &Limiter{client: l.client, prefix: l.prefix, now: now}
}

// Allow spends one token for subject under policy.
func (l *Limiter) Allow(ctx context.Context, policy Policy, subject string) (Decision, error) {
	if policy.Requests <= 0 || policy.Window <= 0 {
		return Decision{Allowed: true}, nil
	}

	capacity := policy.capacity()
	refillPerSecond := float64(policy.Requests) / policy.Window.Seconds()
	// The bucket is kept long enough to refill completely, and no longer.
	ttl := time.Duration(math.Ceil(float64(capacity)/refillPerSecond))*time.Second + policy.Window

	key := fmt.Sprintf("%s:%s:%s", l.prefix, policy.Name, subject)
	result, err := bucketScript.Run(ctx, l.client, []string{key},
		capacity,
		refillPerSecond,
		l.now().UnixMilli(),
		1,
		ttl.Milliseconds(),
	).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(result) != 3 {
		return Decision{}, fmt.Errorf("%w: unexpected script result", ErrUnavailable)
	}

	allowed, _ := result[0].(int64)
	remaining, _ := result[1].(int64)
	retryAfterMS, _ := result[2].(int64)

	decision := Decision{
		Allowed:   allowed == 1,
		Remaining: int(remaining),
	}
	if !decision.Allowed {
		decision.RetryAfter = time.Duration(retryAfterMS) * time.Millisecond
		if decision.RetryAfter < time.Second {
			decision.RetryAfter = time.Second
		}
	}
	return decision, nil
}

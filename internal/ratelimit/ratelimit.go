// Package ratelimit implements per-consumer, per-service-type rate limiting
// backed by Redis fixed-window counters (INCR + EXPIRE via Lua script).
//
// Rate limits are configured in config.yaml under the top-level rate_limits key,
// indexed by service type then user type:
//
//	rate_limits:
//	  audio:
//	    premium:
//	      rate: 100
//	      period: 1m
//	    "*":
//	      rate: 10
//	      period: 1m
//
// The user type is read from a configurable HTTP header (server.user_type_header),
// typically injected by OPA. "*" acts as fallback when the header is absent or
// the specific user type has no configured limit.
//
// Redis key format: rl:{consumer}:{service_type}:{user_type}
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/metrics"
)

// Checker is the interface implemented by Limiter.
// Handlers depend on this interface so they can be tested without Redis.
type Checker interface {
	// Check evaluates the rate limit for the given request and service type.
	// Returns (true, 0, nil) when the request is allowed.
	// Returns (false, retryAfter, nil) when the limit is exceeded.
	// Returns (false, 0, err) on Redis errors — callers should fail open.
	Check(ctx context.Context, r *http.Request, serviceType string) (bool, time.Duration, error)
}

// Limiter checks rate limits using Redis fixed-window counters.
type Limiter struct {
	rdb            *redis.Client
	limits         map[string]map[string]config.RateLimitConfig
	consumerHeader string
	userTypeHeader string
}

// script atomically increments the counter and sets the TTL on first access.
// Returns -1 when the request is allowed, or the remaining TTL (seconds) when denied.
var script = redis.NewScript(`
local key   = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl   = tonumber(ARGV[2])
local count = redis.call('INCR', key)
if count == 1 then
  redis.call('EXPIRE', key, ttl)
end
if count > limit then
  return redis.call('TTL', key)
end
return -1
`)

// New creates a Limiter. Pass the raw *redis.Client (available via storage.RedisClient.Client()).
func New(
	rdb *redis.Client,
	limits map[string]map[string]config.RateLimitConfig,
	consumerHeader, userTypeHeader string,
) *Limiter {
	return &Limiter{
		rdb:            rdb,
		limits:         limits,
		consumerHeader: consumerHeader,
		userTypeHeader: userTypeHeader,
	}
}

// Check evaluates the rate limit for the given request and service type.
func (l *Limiter) Check(ctx context.Context, r *http.Request, serviceType string) (bool, time.Duration, error) {
	typeLimits, ok := l.limits[serviceType]
	if !ok {
		return true, 0, nil
	}

	consumer := "anonymous"
	if l.consumerHeader != "" {
		if v := r.Header.Get(l.consumerHeader); v != "" {
			consumer = v
		}
	}

	userType := "*"
	if l.userTypeHeader != "" {
		if v := r.Header.Get(l.userTypeHeader); v != "" {
			userType = v
		}
	}

	// Exact match first, then "*" fallback.
	rlCfg, ok := typeLimits[userType]
	if !ok {
		rlCfg, ok = typeLimits["*"]
		if !ok {
			return true, 0, nil
		}
	}

	period, err := time.ParseDuration(rlCfg.Period)
	if err != nil {
		return false, 0, fmt.Errorf("invalid rate_limit period %q for service %q: %w", rlCfg.Period, serviceType, err)
	}

	windowSec := int64(math.Ceil(period.Seconds()))
	key := fmt.Sprintf("rl:%s:%s:%s", consumer, serviceType, userType)

	ttl, err := script.Run(ctx, l.rdb, []string{key}, rlCfg.Rate, windowSec).Int64()
	if err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return false, 0, fmt.Errorf("rate limit script: %w", err)
	}

	result := "allowed"
	if ttl != -1 {
		result = "rejected"
	}
	metrics.RateLimitRequestsTotal.WithLabelValues(serviceType, userType, result).Inc()
	if consumer != "anonymous" {
		metrics.RateLimitConsumerHitsTotal.WithLabelValues(serviceType, userType, consumer).Inc()
	}

	if ttl == -1 {
		return true, 0, nil
	}
	return false, time.Duration(ttl) * time.Second, nil
}

package concurrency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/service"
)

// dispatchSlotScript atomically reserves the next available dispatch slot for
// a model across all gateway replicas.
//
// Algorithm:
//   last_ms  = last reserved slot time (0 if key absent)
//   my_slot  = max(now, last_ms + cold_ms)   → queue after previous caller
//   wait_ms  = my_slot - now                 → 0 for first caller
//
// The key TTL is set to cover the wait plus one extra cold_start period so
// the slot reservation expires cleanly if the waiting worker is cancelled.
var dispatchSlotScript = redis.NewScript(`
local key     = KEYS[1]
local now_ms  = tonumber(ARGV[1])
local cold_ms = tonumber(ARGV[2])
local last_ms = tonumber(redis.call("GET", key) or "0")
local my_slot = math.max(now_ms, last_ms + cold_ms)
local wait_ms = my_slot - now_ms
local ttl_ms  = wait_ms + cold_ms + 5000
redis.call("SET", key, tostring(my_slot), "PX", tostring(ttl_ms))
return wait_ms
`)

// DispatchScheduler staggers async inference calls for a single model across
// all gateway replicas by reserving dispatch slots in Redis.
//
// Worker 1 (any replica): slot = now       → wait = 0
// Worker 2 (any replica): slot = now+cold  → wait = cold_start_time
// Worker 3 (any replica): slot = now+2cold → wait = 2×cold_start_time
type DispatchScheduler struct {
	rdb       *redis.Client
	key       string
	coldStart time.Duration
}

// NewDispatchScheduler creates a Redis-backed scheduler for the given model.
// Returns nil when coldStart is zero (staggering disabled).
func NewDispatchScheduler(model string, coldStart time.Duration, rdb *redis.Client) *DispatchScheduler {
	if coldStart <= 0 {
		return nil
	}
	return &DispatchScheduler{
		rdb:       rdb,
		key:       "gateway:dispatch_slot:" + model,
		coldStart: coldStart,
	}
}

// Wait blocks until this caller's reserved slot is reached, then returns.
// On Redis error, proceeds immediately (fail open).
// Returns ctx.Err() if the context is cancelled while waiting.
func (d *DispatchScheduler) Wait(ctx context.Context) error {
	waitMs, err := dispatchSlotScript.Run(ctx, d.rdb,
		[]string{d.key},
		time.Now().UnixMilli(),
		d.coldStart.Milliseconds(),
	).Int64()
	if err != nil || waitMs <= 0 {
		return nil // fail open or no wait needed
	}

	select {
	case <-time.After(time.Duration(waitMs) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ModelDispatchSchedulers holds per-model DispatchSchedulers built from a registry.
type ModelDispatchSchedulers struct {
	schedulers map[string]*DispatchScheduler
}

// NewModelDispatchSchedulers builds schedulers for all async models that have
// ColdStartTime configured. Returns nil when no model has a cold start time.
func NewModelDispatchSchedulers(reg *service.Registry, rdb *redis.Client) *ModelDispatchSchedulers {
	m := make(map[string]*DispatchScheduler)
	for _, def := range reg.All() {
		if def.ColdStartTime > 0 {
			m[def.Model] = NewDispatchScheduler(def.Model, def.ColdStartTime, rdb)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return &ModelDispatchSchedulers{schedulers: m}
}

// Get returns the DispatchScheduler for the given model, or nil if none configured.
func (m *ModelDispatchSchedulers) Get(model string) *DispatchScheduler {
	if m == nil {
		return nil
	}
	return m.schedulers[model]
}

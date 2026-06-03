package concurrency

import (
	"context"
	"sync"
	"time"

	"kevent/gateway/internal/service"
)

// DispatchScheduler staggers async inference calls for a single model by ensuring
// at least ColdStartTime elapses between consecutive dispatches. This gives Knative
// time to warm up a new pod before the next inference request is sent.
//
// Slots are reserved under the lock: each caller immediately advances lastDispatched
// by ColdStartTime, so concurrent callers queue up deterministically without races.
type DispatchScheduler struct {
	coldStart      time.Duration
	mu             sync.Mutex
	lastDispatched time.Time
}

// NewDispatchScheduler creates a scheduler for the given cold start duration.
// Returns nil when coldStart is zero (staggering disabled).
func NewDispatchScheduler(coldStart time.Duration) *DispatchScheduler {
	if coldStart <= 0 {
		return nil
	}
	return &DispatchScheduler{coldStart: coldStart}
}

// Wait blocks until this caller's reserved dispatch slot is reached, then returns.
// Each call serialises callers: the first dispatches immediately, the second waits
// ColdStartTime, the third waits 2×ColdStartTime, etc.
// Returns ctx.Err() if the context is cancelled while waiting.
func (d *DispatchScheduler) Wait(ctx context.Context) error {
	d.mu.Lock()
	now := time.Now()
	// How long until the previous reservation ends.
	wait := d.coldStart - now.Sub(d.lastDispatched)
	if wait > 0 {
		// Reserve a slot: advance lastDispatched to when this caller will dispatch.
		d.lastDispatched = now.Add(wait)
	} else {
		d.lastDispatched = now
		wait = 0
	}
	d.mu.Unlock()

	if wait == 0 {
		return nil
	}
	select {
	case <-time.After(wait):
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
func NewModelDispatchSchedulers(reg *service.Registry) *ModelDispatchSchedulers {
	m := make(map[string]*DispatchScheduler)
	for _, def := range reg.All() {
		if def.ColdStartTime > 0 {
			m[def.Model] = NewDispatchScheduler(def.ColdStartTime)
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

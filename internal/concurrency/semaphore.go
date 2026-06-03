// Package concurrency provides per-model concurrency limiting for sync inference calls.
package concurrency

import "kevent/gateway/internal/service"

// ModelSemaphore limits the number of simultaneous sync proxy calls per model.
// Only models with MaxConcurrentSync > 0 have a slot channel; others are unconstrained.
// Acquire and release are non-blocking — callers receive a bool indicating success.
type ModelSemaphore struct {
	slots map[string]chan struct{}
}

// NewModelSemaphore builds a ModelSemaphore from the registry.
// Models with MaxConcurrentSync == 0 are not tracked (no limit).
// Returns nil when no model in the registry has a limit configured.
func NewModelSemaphore(reg *service.Registry) *ModelSemaphore {
	slots := make(map[string]chan struct{})
	for _, def := range reg.All() {
		if def.MaxConcurrentSync > 0 {
			ch := make(chan struct{}, def.MaxConcurrentSync)
			for i := 0; i < def.MaxConcurrentSync; i++ {
				ch <- struct{}{}
			}
			slots[def.Model] = ch
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return &ModelSemaphore{slots: slots}
}

// TryAcquire attempts to acquire a slot for the given model.
// Returns true (slot taken) or false (all slots busy → caller should return 503).
// If no limit is configured for model, always returns true.
func (s *ModelSemaphore) TryAcquire(model string) bool {
	ch, ok := s.slots[model]
	if !ok {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Release returns a slot for the given model after a request completes.
// No-op when model has no limit configured.
func (s *ModelSemaphore) Release(model string) {
	ch, ok := s.slots[model]
	if !ok {
		return
	}
	ch <- struct{}{}
}

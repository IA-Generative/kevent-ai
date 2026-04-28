package cache

import (
	"context"
	"time"
)

type noopCache struct{}

// NewNoop returns a Cache that never stores anything. Used when TTL is 0.
func NewNoop() Cache { return noopCache{} }

func (noopCache) Get(_ context.Context, _ string) (*Entry, bool, error) { return nil, false, nil }
func (noopCache) Set(_ context.Context, _ string, _ *Entry, _ time.Duration) error { return nil }

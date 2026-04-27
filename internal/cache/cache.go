package cache

import (
	"context"
	"time"
)

// Cache stores and retrieves LLM response entries by an opaque string key.
type Cache interface {
	Get(ctx context.Context, key string) (*Entry, bool, error)
	Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error
}

// Entry holds a cached LLM response.
type Entry struct {
	Body        []byte `json:"body"`
	ContentType string `json:"content_type"`
	StatusCode  int    `json:"status_code"`
}

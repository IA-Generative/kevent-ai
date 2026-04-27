package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisCache struct {
	client *redis.Client
}

// NewRedisCache returns a Cache backed by the provided go-redis client.
// The client is shared with the main RedisClient — no new connection pool is created.
func NewRedisCache(client *redis.Client) Cache {
	return &redisCache{client: client}
}

func (c *redisCache) Get(ctx context.Context, key string) (*Entry, bool, error) {
	data, err := c.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache get %q: %w", key, err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false, fmt.Errorf("cache decode %q: %w", key, err)
	}
	return &entry, true, nil
}

func (c *redisCache) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cache encode %q: %w", key, err)
	}
	if err := c.client.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("cache set %q: %w", key, err)
	}
	return nil
}

package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConsumerTracker records per-consumer token usage in Redis sorted sets and
// periodically refreshes the LLMConsumerTokensTop GaugeVec.
//
// Redis keys: llm:consumer:tokens:<user_type>:<token_type>
// Each key is a sorted set: member = consumerID, score = cumulative token count.
type ConsumerTracker interface {
	Track(ctx context.Context, consumerID, userType, tokenType string, count int)
}

type redisTracker struct {
	client *redis.Client
}

// NewRedisTracker returns a ConsumerTracker backed by Redis sorted sets.
func NewRedisTracker(client *redis.Client) ConsumerTracker {
	return &redisTracker{client: client}
}

func (t *redisTracker) Track(ctx context.Context, consumerID, userType, tokenType string, count int) {
	if consumerID == "" || count == 0 {
		return
	}
	key := fmt.Sprintf("llm:consumer:tokens:%s:%s", userType, tokenType)
	if err := t.client.ZIncrBy(ctx, key, float64(count), consumerID).Err(); err != nil {
		slog.WarnContext(ctx, "consumer tracker: redis ZIncrBy failed", "key", key, "error", err)
	}
}

// StartTopNRefresh launches a background goroutine that reads the top-N consumers
// from Redis sorted sets every refreshInterval and updates LLMConsumerTokensTop.
// The goroutine stops when ctx is cancelled.
func StartTopNRefresh(ctx context.Context, client *redis.Client, topN int, refreshInterval time.Duration) {
	if topN <= 0 {
		return
	}
	go func() {
		refreshTopN(ctx, client, topN) // populate on startup without waiting one full interval
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshTopN(ctx, client, topN)
			}
		}
	}()
}

func refreshTopN(ctx context.Context, client *redis.Client, topN int) {
	userTypes := []string{"sa", "user"}
	tokenTypes := []string{"prompt", "completion"}

	// Collect active label sets from the current GaugeVec before resetting.
	LLMConsumerTokensTop.Reset()

	for _, ut := range userTypes {
		for _, tt := range tokenTypes {
			key := fmt.Sprintf("llm:consumer:tokens:%s:%s", ut, tt)
			results, err := client.ZRevRangeWithScores(ctx, key, 0, int64(topN-1)).Result()
			if err != nil {
				slog.WarnContext(ctx, "consumer tracker: top-N refresh failed", "key", key, "error", err)
				continue
			}
			for _, z := range results {
				consumer, ok := z.Member.(string)
				if !ok {
					continue
				}
				LLMConsumerTokensTop.WithLabelValues(consumer, ut, tt).Set(z.Score)
			}
		}
	}
}

// NoopTracker discards all tracking calls. Used when consumer tracking is disabled.
type NoopTracker struct{}

func (NoopTracker) Track(_ context.Context, _, _, _ string, _ int) {}

var _ ConsumerTracker = NoopTracker{}
var _ ConsumerTracker = (*redisTracker)(nil)

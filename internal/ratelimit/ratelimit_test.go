package ratelimit_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/ratelimit"
)

func newLimiter(t *testing.T, limits map[string]map[string]config.RateLimitConfig, consumerHeader, userTypeHeader string) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb, limits, consumerHeader, userTypeHeader), mr
}

func TestCheck_NoConfiguredService_Allowed(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest("POST", "/", nil)
	allowed, _, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("expected allowed when no config for service type")
	}
}

func TestCheck_UnlimitedRate_AlwaysAllowed(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"unlimited": {Rate: 0},
			"*":         {Rate: 2, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		r.Header.Set("X-User-Type", "unlimited")
		allowed, _, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("iteration %d: expected unlimited user to always be allowed", i)
		}
	}
}

func TestCheck_RateEnforced_AfterLimit(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 3, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 1; i <= 3; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		allowed, _, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	// 4th request must be rejected.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "user1")
	allowed, retryAfter, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("4th request should be rejected")
	}
	if retryAfter <= 0 {
		t.Fatal("expected positive retry-after duration")
	}
}

func TestCheck_ExactMatchBeforeFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"premium": {Rate: 10, Period: "1m"},
			"*":       {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	// premium user gets rate 10, so 2 requests must both be allowed.
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "puser")
		r.Header.Set("X-User-Type", "premium")
		allowed, _, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Fatalf("premium request %d should be allowed", i+1)
		}
	}

	// default user hits limit after 1 request.
	r1 := httptest.NewRequest("POST", "/", nil)
	r1.Header.Set("X-Consumer", "duser")
	allowed1, _, _ := l.Check(context.Background(), r1, "audio")
	if !allowed1 {
		t.Fatal("1st default request should be allowed")
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "duser")
	allowed2, _, _ := l.Check(context.Background(), r2, "audio")
	if allowed2 {
		t.Fatal("2nd default request should be rejected")
	}
}

func TestCheck_NoUserTypeHeader_UsesFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "u1")
	// No X-User-Type header — should fall through to "*".
	allowed, _, err := l.Check(context.Background(), r, "audio")
	if err != nil || !allowed {
		t.Fatalf("first request should pass: allowed=%v err=%v", allowed, err)
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "u1")
	allowed2, _, _ := l.Check(context.Background(), r2, "audio")
	if allowed2 {
		t.Fatal("second request should be rejected by fallback limit")
	}
}

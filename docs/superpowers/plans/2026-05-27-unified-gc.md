# Unified GC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stale-job GC goroutine with a unified GC that sweeps stale-pending jobs AND deletes orphaned S3 objects (input + result) for jobs no longer in Redis.

**Architecture:** Five changes in dependency order — config struct first, then storage layer (S3 list, Redis batch-exists), then GC logic in a new `cmd/gateway/gc.go`, then main.go wiring, then YAML/Helm files. The GC runs as a single goroutine on a 1-minute base ticker; it compares elapsed time against the configured interval so hot-reload can change frequency without a restart.

**Tech Stack:** Go 1.22, aws-sdk-go-v2 v1.96.4 (ListObjectsV2 paginator), go-redis v9, miniredis v2 (tests).

---

### Task 1: Config — rename `pending_max_age_hours` and add `GCConfig`

**Files:**
- Modify: `internal/config/config.go`

- [ ] **Step 1: Update `RedisConfig` — replace int field with string**

In `internal/config/config.go`, replace the `PendingMaxAgeH` field and add a duration helper:

```go
// Before (remove):
type RedisConfig struct {
    Addr           string `yaml:"addr"`
    Password       string `yaml:"password"`
    DB             int    `yaml:"db"`
    PendingMaxAgeH int    `yaml:"pending_max_age_hours"` // 0 = GC disabled
}

// After:
type RedisConfig struct {
    Addr          string `yaml:"addr"`
    Password      string `yaml:"password"`
    DB            int    `yaml:"db"`
    PendingMaxAge string `yaml:"pending_max_age"` // duration string, e.g. "2h"; empty = disabled
}

func (r RedisConfig) PendingMaxAgeDuration() time.Duration { return parseDuration(r.PendingMaxAge) }
```

- [ ] **Step 2: Add `GCConfig` struct and embed it in `LifecycleConfig`**

Add after `JobTTLConfig` helpers:

```go
// GCConfig controls the unified background garbage collector.
type GCConfig struct {
    Enabled      bool   `yaml:"enabled"`       // master switch; default false
    Interval     string `yaml:"interval"`      // tick frequency; default "15m"
    OrphanMinAge string `yaml:"orphan_min_age"` // min S3 object age before orphan check; default "5m"
}

func (g GCConfig) IntervalDuration() time.Duration     { return parseDuration(g.Interval) }
func (g GCConfig) OrphanMinAgeDuration() time.Duration { return parseDuration(g.OrphanMinAge) }
```

In `LifecycleConfig`, add the `GC` field:

```go
type LifecycleConfig struct {
    PersistsResult bool         `yaml:"persists_result"`
    JobTTL         JobTTLConfig `yaml:"job_ttl"`
    GC             GCConfig     `yaml:"gc"`
}
```

- [ ] **Step 3: Update `setDefaults` — remove old default, add new ones**

Replace the old `PendingMaxAgeH` default block:

```go
// Remove:
if c.Redis.PendingMaxAgeH == 0 {
    c.Redis.PendingMaxAgeH = 2
}

// Add:
if c.Redis.PendingMaxAge == "" {
    c.Redis.PendingMaxAge = "2h"
}
if c.Lifecycle.GC.Interval == "" {
    c.Lifecycle.GC.Interval = "15m"
}
if c.Lifecycle.GC.OrphanMinAge == "" {
    c.Lifecycle.GC.OrphanMinAge = "5m"
}
```

- [ ] **Step 4: Verify build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go
git commit -m "refactor(config): rename pending_max_age_hours to pending_max_age, add GCConfig"
```

---

### Task 2: S3 — add `groupByJobID` helper and unit test

**Files:**
- Modify: `internal/storage/s3.go`
- Create: `internal/storage/s3_gc_test.go`

- [ ] **Step 1: Add `S3JobEntry` type and `s3Object` internal type to `s3.go`**

Add before `NewS3Client`:

```go
// S3JobEntry groups all S3 object keys belonging to a single job.
type S3JobEntry struct {
    Keys          []string
    OldestModTime time.Time
}

// s3Object is an internal DTO used by groupByJobID.
type s3Object struct {
    key     string
    modTime time.Time
}
```

- [ ] **Step 2: Add `groupByJobID` pure helper to `s3.go`**

Add after `S3JobEntry`:

```go
// groupByJobID groups S3 objects by their first path segment (the job ID).
// Objects whose key contains no "/" are ignored (not a job file).
// OldestModTime tracks the earliest LastModified across all objects for a job.
func groupByJobID(objects []s3Object) map[string]S3JobEntry {
    result := make(map[string]S3JobEntry)
    for _, obj := range objects {
        idx := strings.IndexByte(obj.key, '/')
        if idx <= 0 {
            continue
        }
        jobID := obj.key[:idx]
        entry := result[jobID]
        entry.Keys = append(entry.Keys, obj.key)
        if entry.OldestModTime.IsZero() || obj.modTime.Before(entry.OldestModTime) {
            entry.OldestModTime = obj.modTime
        }
        result[jobID] = entry
    }
    return result
}
```

Add `"strings"` to the import block in `s3.go`.

- [ ] **Step 3: Write failing test for `groupByJobID`**

Create `internal/storage/s3_gc_test.go`:

```go
package storage

import (
    "testing"
    "time"
)

func TestGroupByJobID(t *testing.T) {
    now := time.Now()
    old := now.Add(-10 * time.Minute)

    objects := []s3Object{
        {key: "abc/input.wav", modTime: old},
        {key: "abc/result.json", modTime: now},
        {key: "def/input.mp3", modTime: old},
        {key: "noslash", modTime: now},  // no "/" — must be skipped
        {key: "/leadingslash", modTime: now}, // empty first segment — must be skipped
    }

    got := groupByJobID(objects)

    if len(got) != 2 {
        t.Fatalf("expected 2 job entries, got %d", len(got))
    }

    abc := got["abc"]
    if len(abc.Keys) != 2 {
        t.Errorf("abc: expected 2 keys, got %d", len(abc.Keys))
    }
    if !abc.OldestModTime.Equal(old) {
        t.Errorf("abc: expected OldestModTime=%v, got %v", old, abc.OldestModTime)
    }

    def := got["def"]
    if len(def.Keys) != 1 {
        t.Errorf("def: expected 1 key, got %d", len(def.Keys))
    }
    if !def.OldestModTime.Equal(old) {
        t.Errorf("def: expected OldestModTime=%v, got %v", old, def.OldestModTime)
    }
}
```

- [ ] **Step 4: Run test — verify it fails (function not yet present)**

```bash
go test ./internal/storage/ -run TestGroupByJobID -v
```

Expected: FAIL or compile error.

- [ ] **Step 5: Run test after Step 2 implementation**

```bash
go test ./internal/storage/ -run TestGroupByJobID -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/s3.go internal/storage/s3_gc_test.go
git commit -m "feat(storage): add groupByJobID helper for S3 orphan detection"
```

---

### Task 3: S3 — add `ListJobObjects` method

**Files:**
- Modify: `internal/storage/s3.go`

- [ ] **Step 1: Add `ListJobObjects` method**

The `aws.ToTime` helper is in the `github.com/aws/aws-sdk-go-v2/aws` package already imported. No new imports needed — `page.Contents` elements expose `Key *string` and `LastModified *time.Time` fields directly.

```go
// ListJobObjects lists all objects in the bucket and groups them by job ID
// (first path segment of the key). Returns a map of jobID → S3JobEntry.
// Objects with no "/" in their key are ignored.
func (c *S3Client) ListJobObjects(ctx context.Context) (map[string]S3JobEntry, error) {
    start := time.Now()
    paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
        Bucket: aws.String(c.bucket),
    })

    var objects []s3Object
    for paginator.HasMorePages() {
        page, err := paginator.NextPage(ctx)
        if err != nil {
            metrics.S3OperationDuration.WithLabelValues("list").Observe(time.Since(start).Seconds())
            metrics.S3ErrorsTotal.WithLabelValues("list").Inc()
            return nil, fmt.Errorf("listing S3 objects: %w", err)
        }
        for _, obj := range page.Contents {
            if obj.Key == nil || obj.LastModified == nil {
                continue
            }
            objects = append(objects, s3Object{
                key:     aws.ToString(obj.Key),
                modTime: *obj.LastModified,
            })
        }
    }
    metrics.S3OperationDuration.WithLabelValues("list").Observe(time.Since(start).Seconds())

    return groupByJobID(objects), nil
}

- [ ] **Step 3: Verify build and existing tests pass**

```bash
go build ./internal/storage/... && go test ./internal/storage/ -v
```

Expected: build OK, all tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/storage/s3.go
git commit -m "feat(storage): add ListJobObjects for S3 orphan GC"
```

---

### Task 4: Redis — add `Ping` and `JobsExistBatch` with tests

**Files:**
- Modify: `internal/storage/redis.go`
- Create: `internal/storage/redis_gc_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/storage/redis_gc_test.go`:

```go
package storage_test

import (
    "context"
    "testing"

    "github.com/alicebob/miniredis/v2"

    "kevent/gateway/internal/config"
    "kevent/gateway/internal/storage"
)

func newTestRedisClient(t *testing.T) (*storage.RedisClient, *miniredis.Miniredis) {
    t.Helper()
    mr := miniredis.RunT(t)
    c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
    if err != nil {
        t.Fatalf("failed to create redis client: %v", err)
    }
    return c, mr
}

func TestJobsExistBatch(t *testing.T) {
    c, mr := newTestRedisClient(t)
    ctx := context.Background()

    mr.Set("job:abc", `{"id":"abc"}`)

    got, err := c.JobsExistBatch(ctx, []string{"abc", "def"})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !got["abc"] {
        t.Error("expected job abc to exist in Redis")
    }
    if got["def"] {
        t.Error("expected job def to be absent from Redis")
    }
}

func TestJobsExistBatch_EmptyInput(t *testing.T) {
    c, _ := newTestRedisClient(t)
    got, err := c.JobsExistBatch(context.Background(), nil)
    if err != nil {
        t.Fatalf("unexpected error on empty input: %v", err)
    }
    if len(got) != 0 {
        t.Errorf("expected empty map, got %v", got)
    }
}

func TestJobsExistBatch_RedisError(t *testing.T) {
    c, mr := newTestRedisClient(t)
    mr.Close()

    _, err := c.JobsExistBatch(context.Background(), []string{"abc"})
    if err == nil {
        t.Error("expected error when Redis is unavailable")
    }
}

func TestPing(t *testing.T) {
    c, _ := newTestRedisClient(t)
    if err := c.Ping(context.Background()); err != nil {
        t.Fatalf("unexpected ping error: %v", err)
    }
}

func TestPing_Unavailable(t *testing.T) {
    c, mr := newTestRedisClient(t)
    mr.Close()
    if err := c.Ping(context.Background()); err == nil {
        t.Error("expected ping error when Redis is down")
    }
}
```

- [ ] **Step 2: Run tests — verify they fail**

```bash
go test ./internal/storage/ -run "TestJobsExistBatch|TestPing" -v
```

Expected: compile error (`JobsExistBatch` and `Ping` undefined).

- [ ] **Step 3: Add `Ping` and `JobsExistBatch` to `redis.go`**

After `func (r *RedisClient) Raw()`:

```go
// Ping checks the Redis connection health. Returns an error if unavailable.
func (r *RedisClient) Ping(ctx context.Context) error {
    return r.client.Ping(ctx).Err()
}

// JobsExistBatch checks which job IDs from ids have a live record in Redis.
// Returns a presence map keyed by job ID. Returns an error if Redis is unavailable —
// callers must treat an error as "inventory unreliable" and skip any deletion.
func (r *RedisClient) JobsExistBatch(ctx context.Context, ids []string) (map[string]bool, error) {
    if len(ids) == 0 {
        return map[string]bool{}, nil
    }
    keys := make([]string, len(ids))
    for i, id := range ids {
        keys[i] = jobKey(id)
    }
    vals, err := r.client.MGet(ctx, keys...).Result()
    if err != nil {
        return nil, fmt.Errorf("redis MGET for batch job existence check: %w", err)
    }
    result := make(map[string]bool, len(ids))
    for i, v := range vals {
        result[ids[i]] = v != nil
    }
    return result, nil
}
```

- [ ] **Step 4: Run tests — verify they pass**

```bash
go test ./internal/storage/ -run "TestJobsExistBatch|TestPing" -v
```

Expected: all PASS.

- [ ] **Step 5: Run full test suite**

```bash
go test ./...
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/redis.go internal/storage/redis_gc_test.go
git commit -m "feat(storage): add Ping and JobsExistBatch for unified GC"
```

---

### Task 5: GC logic — new `cmd/gateway/gc.go`

**Files:**
- Create: `cmd/gateway/gc.go`

- [ ] **Step 1: Create `cmd/gateway/gc.go` with `runGC` function**

```go
package main

import (
    "context"
    "log/slog"
    "time"

    "kevent/gateway/internal/metrics"
    "kevent/gateway/internal/storage"
)

// runGC executes one GC cycle with two phases:
//   - Phase 1: sweep stale-pending jobs (maxAge > 0 only)
//   - Phase 2: delete orphaned S3 objects for jobs absent from Redis
//
// Phase 2 aborts early if Redis is unavailable or returns errors, to avoid
// deleting files whose jobs simply couldn't be verified.
func runGC(ctx context.Context, redis *storage.RedisClient, s3Client *storage.S3Client, maxAge, orphanMinAge time.Duration) {
    // ── Phase 1: stale-pending sweep ─────────────────────────────────────────
    if maxAge > 0 {
        swept, err := redis.SweepStalePendingJobs(ctx, maxAge)
        if err != nil {
            slog.Error("GC phase1: stale-pending sweep failed", "error", err)
        } else {
            for _, job := range swept {
                metrics.AsyncStaleJobsSweptTotal.WithLabelValues(job.Model).Inc()
                if job.InputRef == "" {
                    continue
                }
                inputRef := job.InputRef
                jobID := job.ID
                go func() {
                    if err := s3Client.DeleteObject(context.Background(), inputRef); err != nil {
                        slog.Error("GC phase1: failed to delete stale input", "job_id", jobID, "input_ref", inputRef, "error", err)
                    }
                }()
            }
            if len(swept) > 0 {
                slog.Info("GC phase1: swept stale-pending jobs", "count", len(swept))
            }
        }
    }

    // ── Phase 2: orphan S3 cleanup ────────────────────────────────────────────
    // Safeguard: abort if Redis is unavailable — we cannot distinguish "job gone"
    // from "Redis down" without a reliable ping.
    if err := redis.Ping(ctx); err != nil {
        slog.Error("GC phase2: Redis unavailable, skipping S3 orphan cleanup", "error", err)
        return
    }

    jobObjects, err := s3Client.ListJobObjects(ctx)
    if err != nil {
        slog.Error("GC phase2: S3 listing failed, skipping orphan cleanup", "error", err)
        return
    }

    // Collect job IDs whose oldest S3 object predates the orphan_min_age window.
    var candidates []string
    for jobID, entry := range jobObjects {
        if time.Since(entry.OldestModTime) >= orphanMinAge {
            candidates = append(candidates, jobID)
        }
    }
    if len(candidates) == 0 {
        return
    }

    exists, err := redis.JobsExistBatch(ctx, candidates)
    if err != nil {
        slog.Error("GC phase2: Redis inventory unreliable, skipping S3 orphan cleanup", "error", err)
        return
    }

    orphans := 0
    for _, jobID := range candidates {
        if exists[jobID] {
            continue
        }
        orphans++
        for _, key := range jobObjects[jobID].Keys {
            if err := s3Client.DeleteObject(ctx, key); err != nil {
                slog.Error("GC phase2: failed to delete orphan S3 object", "job_id", jobID, "key", key, "error", err)
            } else {
                slog.Info("GC phase2: deleted orphan S3 object", "job_id", jobID, "key", key)
            }
        }
    }
    if orphans > 0 {
        slog.Info("GC phase2: orphan S3 cleanup complete", "orphans", orphans)
    }
}
```

- [ ] **Step 2: Build to verify**

```bash
go build ./cmd/gateway/...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add cmd/gateway/gc.go
git commit -m "feat(gc): add runGC with stale-pending sweep and S3 orphan cleanup"
```

---

### Task 6: Wire GC into `main.go`

**Files:**
- Modify: `cmd/gateway/main.go`

- [ ] **Step 1: Replace `gcMaxAge atomic.Int64` declaration with four atomics**

Find the existing declaration:

```go
// gcMaxAge is declared here so reloadFn can update it before the GC goroutine starts.
var gcMaxAge atomic.Int64
```

Replace with:

```go
// GC atomics — declared before reloadFn so the reload path can update them.
var (
    gcEnabled      atomic.Bool
    gcInterval     atomic.Int64 // nanoseconds
    gcOrphanMinAge atomic.Int64 // nanoseconds
    gcMaxAge       atomic.Int64 // nanoseconds
)
```

- [ ] **Step 2: Update `reloadFn` — replace old `gcMaxAge.Store` with new stores**

Find in `reloadFn`:

```go
gcMaxAge.Store(int64(time.Duration(newCfg.Redis.PendingMaxAgeH) * time.Hour))
```

Replace with:

```go
gcEnabled.Store(newCfg.Lifecycle.GC.Enabled)
if iv := newCfg.Lifecycle.GC.IntervalDuration(); iv > 0 {
    gcInterval.Store(int64(iv))
}
if oma := newCfg.Lifecycle.GC.OrphanMinAgeDuration(); oma > 0 {
    gcOrphanMinAge.Store(int64(oma))
}
gcMaxAge.Store(int64(newCfg.Redis.PendingMaxAgeDuration()))
```

- [ ] **Step 3: Replace the stale-job GC goroutine block**

Find the entire block from:
```go
// ── Stale-job garbage collector ───────────────────────────────────────────
```
to:
```go
if cfg.Redis.PendingMaxAgeH > 0 {
    slog.Info("stale-job GC enabled", "max_age_hours", cfg.Redis.PendingMaxAgeH)
}
```

Replace it with:

```go
// ── Unified GC ────────────────────────────────────────────────────────────
// All atomics are read on each tick so hot-reload takes effect without restart.
gcEnabled.Store(cfg.Lifecycle.GC.Enabled)
iv := cfg.Lifecycle.GC.IntervalDuration()
if iv <= 0 {
    iv = 15 * time.Minute
}
gcInterval.Store(int64(iv))
oma := cfg.Lifecycle.GC.OrphanMinAgeDuration()
if oma <= 0 {
    oma = 5 * time.Minute
}
gcOrphanMinAge.Store(int64(oma))
gcMaxAge.Store(int64(cfg.Redis.PendingMaxAgeDuration()))

go func() {
    ticker := time.NewTicker(time.Minute)
    defer ticker.Stop()
    var lastRun time.Time
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if !gcEnabled.Load() {
                continue
            }
            interval := time.Duration(gcInterval.Load())
            if interval <= 0 {
                interval = 15 * time.Minute
            }
            if !lastRun.IsZero() && time.Since(lastRun) < interval {
                continue
            }
            lastRun = time.Now()
            runGC(ctx, redisClient, s3Client,
                time.Duration(gcMaxAge.Load()),
                time.Duration(gcOrphanMinAge.Load()),
            )
        }
    }
}()

if cfg.Lifecycle.GC.Enabled {
    slog.Info("unified GC enabled",
        "interval", cfg.Lifecycle.GC.Interval,
        "pending_max_age", cfg.Redis.PendingMaxAge,
        "orphan_min_age", cfg.Lifecycle.GC.OrphanMinAge,
    )
}
```

- [ ] **Step 4: Build and run tests**

```bash
go build ./cmd/gateway/ && go test ./...
```

Expected: build OK, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/gateway/main.go
git commit -m "feat(main): wire unified GC goroutine, replace stale-job GC"
```

---

### Task 7: Update config files

**Files:**
- Modify: `config.yaml`
- Modify: `helm/gateway/values.yaml`
- Modify: `helm/gateway/templates/configmap.yaml`

- [ ] **Step 1: Update `config.yaml` — rename field and add gc block**

In the `redis:` section, replace:

```yaml
  # pending_max_age_hours: mark pending jobs as failed (reason: stale) after N hours with no relay pickup.
  # Set to 0 to disable the garbage collector. Default: 2h.
  # ⚠ Must be set LOWER than your S3 staging input TTL: the GC marks jobs failed
  # and deletes their input files before S3 purges them. If the GC runs after S3
  # purges the input, the relay will pick up the job and fail with "input file not found".
  pending_max_age_hours: ${PENDING_MAX_AGE_HOURS:-2}
```

With:

```yaml
  # pending_max_age: mark pending jobs as failed (stale) after this duration with no relay pickup.
  # Empty or "0s" disables stale-pending sweep. Default: 2h.
  # ⚠ Must be shorter than the S3 input staging TTL — the GC deletes input files when marking
  # jobs stale. If the GC runs after S3 has already purged the input, the relay gets "file not found".
  pending_max_age: "${PENDING_MAX_AGE:-2h}"
```

In the `lifecycle:` section, add the `gc:` block after `job_ttl:`:

```yaml
  # gc: unified background garbage collector.
  # enabled: false by default — set to true to activate.
  # interval: how often the GC runs (default 15m).
  # orphan_min_age: S3 objects younger than this are never treated as orphans (default 5m).
  gc:
    enabled: ${LIFECYCLE_GC_ENABLED:-false}
    interval: "${LIFECYCLE_GC_INTERVAL:-15m}"
    orphan_min_age: "${LIFECYCLE_GC_ORPHAN_MIN_AGE:-5m}"
```

- [ ] **Step 2: Update `helm/gateway/values.yaml` — rename field and add gc block**

In the `lifecycle:` section, replace the comment block:

```yaml
lifecycle:
  # When true, S3 results and Redis records persist until their TTL expires.
  # When false (default), cleanup is immediate on first GET or webhook delivery.
  persistsResult: false
  jobTTL:
    # global: fallback TTL for all job statuses. Example: "24h", "72h"
    global: ""
    # Per-status overrides (take precedence over global):
    # completed: ""
    # pending:   ""
    # failed:    ""
```

With:

```yaml
lifecycle:
  # When true, S3 results and Redis records persist until their TTL expires.
  # When false (default), cleanup is immediate on first GET or webhook delivery.
  persistsResult: false
  jobTTL:
    # global: fallback TTL for all job statuses. Example: "24h", "72h"
    global: ""
    # Per-status overrides (take precedence over global):
    # completed: ""
    # pending:   ""
    # failed:    ""
  gc:
    # enabled: set to true to activate the unified GC
    enabled: false
    # interval: how often the GC runs. Default "15m".
    interval: "15m"
    # orphanMinAge: S3 objects younger than this are never treated as orphans. Default "5m".
    orphanMinAge: "5m"
```

`pending_max_age_hours` is not in `values.yaml` — it is managed via env var expansion in `config.yaml` only. No change needed in values.yaml for this field.

- [ ] **Step 3: Update `helm/gateway/templates/configmap.yaml` — render new fields**

In the `redis:` block of the configmap template, replace the old field render if present. Then in the `lifecycle:` block, add after `job_ttl:`:

```yaml
    lifecycle:
      persists_result: {{ .Values.lifecycle.persistsResult | default false }}
      job_ttl:
        global: "{{ .Values.lifecycle.jobTTL.global | default "" }}"
        {{- if .Values.lifecycle.jobTTL.completed }}
        completed: "{{ .Values.lifecycle.jobTTL.completed }}"
        {{- end }}
        {{- if .Values.lifecycle.jobTTL.pending }}
        pending: "{{ .Values.lifecycle.jobTTL.pending }}"
        {{- end }}
        {{- if .Values.lifecycle.jobTTL.failed }}
        failed: "{{ .Values.lifecycle.jobTTL.failed }}"
        {{- end }}
      gc:
        enabled: {{ .Values.lifecycle.gc.enabled | default false }}
        interval: "{{ .Values.lifecycle.gc.interval | default "15m" }}"
        orphan_min_age: "{{ .Values.lifecycle.gc.orphanMinAge | default "5m" }}"
```

- [ ] **Step 4: Build and run all tests**

```bash
go build ./... && go test ./...
```

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add config.yaml helm/gateway/values.yaml helm/gateway/templates/configmap.yaml
git commit -m "feat(config): add lifecycle.gc block, rename pending_max_age_hours to pending_max_age"
```

---

### Task 8: Final verification

- [ ] **Step 1: Run full test suite**

```bash
go test ./... -count=1
```

Expected: all pass, no skips.

- [ ] **Step 2: Verify `go vet`**

```bash
go vet ./...
```

Expected: no issues.

- [ ] **Step 3: Check miniredis is now a direct dependency**

```bash
go mod tidy
git diff go.mod go.sum
```

Expected: `miniredis/v2` moved from `// indirect` to direct, or no change if already direct from ratelimit.

- [ ] **Step 4: Commit if go.mod changed**

```bash
git add go.mod go.sum
git commit -m "chore: tidy go.mod after adding storage tests"
```

- [ ] **Step 5: Final build check**

```bash
go build ./cmd/gateway/
```

Expected: binary builds successfully.

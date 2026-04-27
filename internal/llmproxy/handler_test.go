package llmproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"kevent/gateway/internal/cache"
	"kevent/gateway/internal/llmproxy/provider"
	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/service"
)

// ── in-memory cache ──────────────────────────────────────────────────────────

type memCache struct {
	mu   sync.Mutex
	data map[string]*cache.Entry
}

func newMemCache() *memCache { return &memCache{data: make(map[string]*cache.Entry)} }

func (m *memCache) Get(_ context.Context, key string) (*cache.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	return e, ok, nil
}

func (m *memCache) Set(_ context.Context, key string, entry *cache.Entry, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = entry
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func llmDef(provider, backendModel string, cacheTTL time.Duration) *service.Def {
	return &service.Def{
		Type:             "llm",
		Model:            "my-alias",
		Provider:         provider,
		BackendModel:     backendModel,
		ResponseCacheTTL: cacheTTL,
		InferenceURL:     "", // set per-test via httptest
	}
}

func doServeJSON(h *Handler, def *service.Def, body string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	return doServeJSONAs(h, def, body, "", extraHeaders...)
}

func doServeJSONAs(h *Handler, def *service.Def, body, consumer string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, fn := range extraHeaders {
		fn(req)
	}
	rr := httptest.NewRecorder()
	h.ServeJSON(rr, req, def, []byte(body), consumer)
	return rr
}

const chatBody = `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}]}`

// openAI-format success response used by fake backends.
const fakeResponse = `{"id":"chatcmpl-1","object":"chat.completion","model":"my-alias","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

// ── tests ────────────────────────────────────────────────────────────────────

func TestServeJSON_CacheMiss_ThenHit(t *testing.T) {
	// Fake backend returns a valid response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 60*time.Second)
	def.InferenceURL = backend.URL

	// First call: cache miss.
	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first call: expected X-Cache=MISS, got %q", rr.Header().Get("X-Cache"))
	}

	// Second call: cache hit.
	rr2 := doServeJSON(h, def, chatBody)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rr2.Code)
	}
	if rr2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second call: expected X-Cache=HIT, got %q", rr2.Header().Get("X-Cache"))
	}
	if rr2.Body.String() != rr.Body.String() {
		t.Error("cached body should match original response")
	}
}

func TestServeJSON_NoCacheHeader_BypassesCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 60*time.Second)
	def.InferenceURL = backend.URL

	setNoCache := func(r *http.Request) { r.Header.Set("Cache-Control", "no-cache") }

	doServeJSON(h, def, chatBody, setNoCache)
	doServeJSON(h, def, chatBody, setNoCache)

	if callCount != 2 {
		t.Errorf("Cache-Control: no-cache should bypass cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_Non200NotCached(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 60*time.Second)
	def.InferenceURL = backend.URL

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("429 responses should not be cached; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_CacheDisabled_WhenTTLZero(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 0) // TTL=0 → no cache
	def.InferenceURL = backend.URL

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("TTL=0 should disable cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_BackendModel_RewrittenInRequest(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "meta-llama/Meta-Llama-3-8B-Instruct", 0)
	def.InferenceURL = backend.URL

	// Client sends alias "my-alias".
	doServeJSON(h, def, chatBody)

	if receivedModel != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("backend should receive backend_model; got %q", receivedModel)
	}
}

func TestServeJSON_BackendModel_NotRewritten_WhenEmpty(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 0) // no backend_model
	def.InferenceURL = backend.URL

	doServeJSON(h, def, chatBody)

	// When BackendModel is empty, the alias from the body is forwarded as-is.
	if receivedModel != "my-alias" {
		t.Errorf("without backend_model, original model should be forwarded; got %q", receivedModel)
	}
}

func TestServeJSON_CacheHit_ReturnsCachedBody(t *testing.T) {
	mc := newMemCache()

	// Pre-populate cache with a known entry.
	cacheKey, cacheable, err := cache.Key("passthrough", "my-alias", []byte(chatBody))
	if err != nil || !cacheable {
		t.Fatalf("test setup: cache.Key failed: err=%v cacheable=%v", err, cacheable)
	}
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"cached":true}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 60*time.Second)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "HIT" {
		t.Errorf("expected X-Cache=HIT, got %q", rr.Header().Get("X-Cache"))
	}
	if rr.Body.String() != `{"cached":true}` {
		t.Errorf("expected cached body, got %q", rr.Body.String())
	}
}

func TestServeJSON_UnknownProvider_Returns500(t *testing.T) {
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second})

	def := &service.Def{
		Type:     "llm",
		Model:    "x",
		Provider: "nonexistent-provider",
	}

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for unknown provider, got %d", rr.Code)
	}
}

func TestRewriteBodyModel(t *testing.T) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	out, err := rewriteBodyModel(body, "real-model-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)

	if result["model"] != "real-model-id" {
		t.Errorf("expected model=real-model-id, got %q", result["model"])
	}
	// Other fields must be preserved.
	if result["temperature"] != 0.5 {
		t.Errorf("expected temperature=0.5, got %v", result["temperature"])
	}
}

func TestRewriteBodyModel_InvalidJSON(t *testing.T) {
	_, err := rewriteBodyModel([]byte(`not json`), "model")
	if err == nil {
		t.Error("expected error for invalid JSON body")
	}
}

// ── consumer metrics ─────────────────────────────────────────────────────────

func TestServeJSON_ConsumerMetrics_EmittedOnBackendResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Response with known token counts.
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 0)
	def.InferenceURL = backend.URL

	// Prometheus counters are global — read before and after to check the delta.
	before := consumerTokens(def, "alice", "prompt")
	doServeJSONAs(h, def, chatBody, "alice")
	after := consumerTokens(def, "alice", "prompt")

	if after-before != 7 {
		t.Errorf("expected +7 prompt tokens for alice, got delta=%v", after-before)
	}
}

func TestServeJSON_ConsumerMetrics_EmittedOnCacheHit(t *testing.T) {
	mc := newMemCache()
	cacheKey, _, _ := cache.Key("passthrough", "my-alias", []byte(chatBody))
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 60*time.Second)

	before := consumerTokens(def, "bob", "completion")
	doServeJSONAs(h, def, chatBody, "bob")
	after := consumerTokens(def, "bob", "completion")

	if after-before != 2 {
		t.Errorf("expected +2 completion tokens for bob on cache hit, got delta=%v", after-before)
	}
}

func TestServeJSON_ConsumerMetrics_SkippedWhenNoConsumer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second})

	def := llmDef("passthrough", "", 0)
	def.InferenceURL = backend.URL

	// Empty consumer — should not panic or emit consumer metrics.
	rr := doServeJSONAs(h, def, chatBody, "")
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// consumerTokens returns the current value of the consumer token counter for
// the given def, consumer, and token type. Tests use before/after deltas to
// assert on increments without resetting global Prometheus state.
func consumerTokens(def *service.Def, consumer, tokenType string) float64 {
	return testutil.ToFloat64(
		metrics.LLMConsumerTokensTotal.WithLabelValues(def.Type, def.Model, consumer, tokenType),
	)
}

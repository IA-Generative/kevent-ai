package service_test

import (
	"testing"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/service"
)

func baseServiceConfig() config.ServiceConfig {
	return config.ServiceConfig{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
			"translation":   {"/v1/audio/translations"},
		},
		InferenceURL:  "http://inference.svc.cluster.local",
		InputTopic:    "jobs.whisper-large-v3.input",
		ResultTopic:   "jobs.whisper-large-v3.results",
		SyncTopic:     "jobs.whisper-large-v3.sync",
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}
}

// TestDef_SyncTopicPopulated verifies that SyncTopic from config is exposed in the Def.
func TestDef_SyncTopicPopulated(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	def, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}

	if def.SyncTopic != "jobs.whisper-large-v3.sync" {
		t.Errorf("expected SyncTopic %q, got %q", "jobs.whisper-large-v3.sync", def.SyncTopic)
	}
}

// TestRegistry_IndexedWithoutInferenceURL verifies that a service configured with
// only a sync_topic (no inference_url) is still indexed for sync routing.
func TestRegistry_IndexedWithoutInferenceURL(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.InferenceURL = "" // no direct proxy — only Kafka sync path

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	def, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("service with only SyncTopic should be routable: %v", err)
	}
	if def.SyncTopic == "" {
		t.Error("SyncTopic should be set")
	}
}

// TestRegistry_NotIndexedWithoutModelOrTopic verifies that a service without a
// model is not indexed for sync routing.
func TestRegistry_NotIndexedWithoutModel(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Model = ""
	cfg.Operations = map[string][]string{"transcription": {"/v1/audio/transcriptions"}}

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	if reg.HasSyncServices() {
		t.Error("service without model should not be indexed for sync")
	}
}

// TestRouteSync_UnknownPathReturnsError verifies that an unknown path returns an error.
func TestRouteSync_UnknownPathReturnsError(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	_, err := reg.RouteSync("/v1/unknown", "whisper-large-v3")
	if err == nil {
		t.Error("expected error for unknown path")
	}
}

// TestRouteSync_UnknownModelReturnsError verifies that an unknown model returns an error.
func TestRouteSync_UnknownModelReturnsError(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	_, err := reg.RouteSync("/v1/audio/transcriptions", "unknown-model")
	if err == nil {
		t.Error("expected error for unknown model")
	}
}

// TestRegistry_MultipleServices verifies routing across two service types.
func TestRegistry_MultipleServices(t *testing.T) {
	cfgs := []config.ServiceConfig{
		baseServiceConfig(),
		{
			Type:  "ocr",
			Model: "llava-v1.6-mistral-7b",
			Operations: map[string][]string{
				"chat": {"/v1/chat/completions"},
			},
			InferenceURL: "http://ocr.svc.cluster.local",
			InputTopic:   "jobs.llava.input",
			ResultTopic:  "jobs.llava.results",
			// No SyncTopic — JSON-based, uses direct proxy
		},
	}
	reg := service.NewRegistry(cfgs)

	def, err := reg.RouteSync("/v1/chat/completions", "llava-v1.6-mistral-7b")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.SyncTopic != "" {
		t.Errorf("OCR should have empty SyncTopic, got %q", def.SyncTopic)
	}

	def2, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def2.SyncTopic == "" {
		t.Error("transcription should have non-empty SyncTopic")
	}
}

// TestRouteSync_PatternPath_ModelInURL verifies that a path pattern like
// "/v2/models/{model}/infer" routes correctly by extracting the model from the URL.
func TestRouteSync_PatternPath_ModelInURL(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"infer": {"/v2/models/{model}/infer"}}
	cfg.SyncTopic = ""

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	def, err := reg.RouteSync("/v2/models/whisper-large-v3/infer", "")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected model whisper-large-v3, got %q", def.Model)
	}
}

// TestRouteSync_PatternPath_SuffixSeparator verifies patterns like
// "/v1/models/{model}:predict" where the model is embedded with a suffix.
func TestRouteSync_PatternPath_SuffixSeparator(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"predict": {"/v1/models/{model}:predict"}}
	cfg.SyncTopic = ""

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	def, err := reg.RouteSync("/v1/models/whisper-large-v3:predict", "")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected model whisper-large-v3, got %q", def.Model)
	}
}

// TestRouteSync_PatternPath_UnknownModelReturnsError verifies that a pattern
// path with an unregistered model name returns an error.
func TestRouteSync_PatternPath_UnknownModelReturnsError(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"infer": {"/v2/models/{model}/infer"}}
	cfg.SyncTopic = ""

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	_, err := reg.RouteSync("/v2/models/unknown-model/infer", "")
	if err == nil {
		t.Error("expected error for unregistered model in pattern path")
	}
}

// TestSyncPathPrefixes verifies that SyncPathPrefixes returns unique prefixes
// for all registered paths (exact and pattern).
func TestSyncPathPrefixes(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
				"infer":         {"/v2/models/{model}/infer"},
			},
			InferenceURL: "http://inference.example.com",
			InputTopic:   "jobs.whisper-large-v3.input",
			ResultTopic:  "jobs.whisper-large-v3.results",
		},
	}
	reg := service.NewRegistry(cfgs)

	prefixes := reg.SyncPathPrefixes()
	prefixSet := make(map[string]struct{}, len(prefixes))
	for _, p := range prefixes {
		prefixSet[p] = struct{}{}
	}

	if _, ok := prefixSet["/v1"]; !ok {
		t.Error("expected /v1 prefix")
	}
	if _, ok := prefixSet["/v2"]; !ok {
		t.Error("expected /v2 prefix")
	}
}

// TestRouteAsync_SyncTopicPreserved verifies that RouteAsync also exposes SyncTopic.
func TestRouteAsync_SyncTopicPreserved(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	def, err := reg.RouteAsync("transcription", "whisper-large-v3")
	if err != nil {
		t.Fatalf("RouteAsync failed: %v", err)
	}
	if def.SyncTopic != "jobs.whisper-large-v3.sync" {
		t.Errorf("expected SyncTopic in RouteAsync result, got %q", def.SyncTopic)
	}
}

// TestRouteAsync_DefaultModel verifies that the default model is used when no
// model is specified and multiple models are configured for the type.
func TestRouteAsync_DefaultModel(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:        "transcription",
			Model:       "whisper-large-v3",
			Default:     true,
			Operations:  map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://whisper-large.svc",
			InputTopic:  "jobs.whisper-large-v3.input",
			ResultTopic: "jobs.whisper-large-v3.results",
		},
		{
			Type:        "transcription",
			Model:       "whisper-turbo",
			Default:     false,
			Operations:  map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://whisper-turbo.svc",
			InputTopic:  "jobs.whisper-turbo.input",
			ResultTopic: "jobs.whisper-turbo.results",
		},
	}
	reg := service.NewRegistry(cfgs)

	// Async: no model specified → default selected.
	def, err := reg.RouteAsync("transcription", "")
	if err != nil {
		t.Fatalf("RouteAsync with default model failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected default model whisper-large-v3, got %q", def.Model)
	}

	// Sync: no model in body → default selected.
	def, err = reg.RouteSync("/v1/audio/transcriptions", "")
	if err != nil {
		t.Fatalf("RouteSync with default model failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected default model whisper-large-v3 for sync, got %q", def.Model)
	}
}

// TestRouteAsync_NoDefaultMultipleModels verifies that omitting model without a
// default configured returns an error.
func TestRouteAsync_NoDefaultMultipleModels(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type: "transcription", Model: "whisper-large-v3",
			Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InputTopic: "j.input", ResultTopic: "j.results",
		},
		{
			Type: "transcription", Model: "whisper-turbo",
			Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InputTopic: "j2.input", ResultTopic: "j2.results",
		},
	}
	reg := service.NewRegistry(cfgs)

	_, err := reg.RouteAsync("transcription", "")
	if err == nil {
		t.Error("expected error when multiple models and no default")
	}
}

// ── wildcard routing ─────────────────────────────────────────────────────────

func wildcardLLMConfig(model string, isDefault bool) config.ServiceConfig {
	return config.ServiceConfig{
		Type:         "llm",
		Model:        model,
		Default:      isDefault,
		Provider:     "passthrough",
		InferenceURL: "http://vllm.svc:8000",
		Operations:   map[string][]string{"proxy": {"/v1/*"}},
	}
}

func TestRouteSync_Wildcard_MatchesAnySubPath(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	paths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/responses",
		"/v1/audio/speech",
	}
	for _, p := range paths {
		def, err := reg.RouteSync(p, "gpt-4o")
		if err != nil {
			t.Errorf("RouteSync(%q) failed: %v", p, err)
			continue
		}
		if def.Model != "gpt-4o" {
			t.Errorf("RouteSync(%q): expected model gpt-4o, got %q", p, def.Model)
		}
	}
}

func TestRouteSync_Wildcard_DoesNotMatchDifferentPrefix(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	_, err := reg.RouteSync("/v2/chat/completions", "gpt-4o")
	if err == nil {
		t.Error("wildcard /v1/* should not match /v2/chat/completions")
	}
}

func TestRouteSync_Wildcard_ExactPathTakesPrecedence(t *testing.T) {
	// Exact path for audio + wildcard for LLM on the same prefix.
	cfgs := []config.ServiceConfig{
		{
			Type:         "audio",
			Model:        "whisper-large-v3",
			Provider:     "",
			InferenceURL: "http://whisper.svc",
			SyncTopic:    "jobs.whisper.sync",
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
		wildcardLLMConfig("gpt-4o", false),
	}
	reg := service.NewRegistry(cfgs)

	// Exact path → whisper, not gpt-4o.
	def, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("exact path should route to whisper: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected whisper-large-v3, got %q", def.Model)
	}

	// Other /v1/* path → gpt-4o via wildcard.
	def, err = reg.RouteSync("/v1/chat/completions", "gpt-4o")
	if err != nil {
		t.Fatalf("wildcard should catch /v1/chat/completions: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %q", def.Model)
	}
}

func TestRouteSync_Wildcard_DefaultModel(t *testing.T) {
	cfgs := []config.ServiceConfig{
		wildcardLLMConfig("gpt-4o", true),
		wildcardLLMConfig("gpt-4o-mini", false),
	}
	reg := service.NewRegistry(cfgs)

	// No model specified → default selected.
	def, err := reg.RouteSync("/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("wildcard default model resolution failed: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected default model gpt-4o, got %q", def.Model)
	}
}

func TestRouteSync_Wildcard_SingleModel_AutoSelected(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	// Single model → auto-selected when no model specified.
	def, err := reg.RouteSync("/v1/embeddings", "")
	if err != nil {
		t.Fatalf("single wildcard model should be auto-selected: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %q", def.Model)
	}
}

func TestRegistry_HasSyncServices_WildcardOnly(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})
	if !reg.HasSyncServices() {
		t.Error("registry with only wildcard routes should report HasSyncServices=true")
	}
}

func TestRegistry_SyncPaths_IncludesWildcard(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})
	paths := reg.SyncPaths()
	found := false
	for _, p := range paths {
		if p == "/v1/*" {
			found = true
		}
	}
	if !found {
		t.Errorf("SyncPaths should include /v1/*; got %v", paths)
	}
}

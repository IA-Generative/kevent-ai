package service_test

import (
	"testing"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/service"
)

func baseServiceConfig() config.ServiceConfig {
	return config.ServiceConfig{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		OpenAIPaths:   []string{"/v1/audio/transcriptions", "/v1/audio/translations"},
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
	cfg.OpenAIPaths = []string{"/v1/audio/transcriptions"}

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
			Type:         "ocr",
			Model:        "llava-v1.6-mistral-7b",
			OpenAIPaths:  []string{"/v1/chat/completions"},
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
	cfg.OpenAIPaths = []string{"/v2/models/{model}/infer"}
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
	cfg.OpenAIPaths = []string{"/v1/models/{model}:predict"}
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
	cfg.OpenAIPaths = []string{"/v2/models/{model}/infer"}
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
			Type:         "transcription",
			Model:        "whisper-large-v3",
			OpenAIPaths:  []string{"/v1/audio/transcriptions", "/v2/models/{model}/infer"},
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

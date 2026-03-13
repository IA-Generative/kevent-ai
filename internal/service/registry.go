// Package service holds the inference service registry.
// Adding a new inference type (e.g. "translation") requires only a new entry
// in config.yaml — no Go code modification.
package service

import (
	"fmt"
	"path/filepath"
	"strings"

	"kevent/gateway/internal/config"
)

// Def describes a registered inference service type.
type Def struct {
	Type          string
	Model         string
	InputTopic    string
	ResultTopic   string
	AcceptedExts  map[string]struct{} // empty = accept any extension
	MaxFileSizeMB int64

	// Sync / OpenAI-compatible mode (optional).
	InferenceURL string   // full URL of the backend endpoint (direct proxy fallback)
	OpenAIPaths  []string // e.g. ["/v1/audio/transcriptions", "/v1/audio/translations"]
	SyncTopic    string   // Kafka topic for priority sync-over-Kafka jobs (overrides direct proxy)
}

// Registry maps (service_type, model) pairs to their runtime definitions.
type Registry struct {
	byTypeModel map[string]map[string]*Def // type → model → Def
	bySync      map[string]map[string]*Def // openai_path → model → Def
}

func NewRegistry(cfgs []config.ServiceConfig) *Registry {
	r := &Registry{
		byTypeModel: make(map[string]map[string]*Def, len(cfgs)),
		bySync:      make(map[string]map[string]*Def),
	}
	for _, cfg := range cfgs {
		exts := make(map[string]struct{}, len(cfg.AcceptedExts))
		for _, ext := range cfg.AcceptedExts {
			exts[strings.ToLower(ext)] = struct{}{}
		}
		def := &Def{
			Type:          cfg.Type,
			Model:         cfg.Model,
			InputTopic:    cfg.InputTopic,
			ResultTopic:   cfg.ResultTopic,
			AcceptedExts:  exts,
			MaxFileSizeMB: cfg.MaxFileSizeMB,
			InferenceURL:  cfg.InferenceURL,
			OpenAIPaths:   cfg.OpenAIPaths,
			SyncTopic:     cfg.SyncTopic,
		}

		if r.byTypeModel[cfg.Type] == nil {
			r.byTypeModel[cfg.Type] = make(map[string]*Def)
		}
		r.byTypeModel[cfg.Type][cfg.Model] = def

		// Build the sync routing index — one entry per configured path.
		// Index when either a direct proxy URL or a sync Kafka topic is configured.
		if cfg.Model != "" && (cfg.InferenceURL != "" || cfg.SyncTopic != "") {
			for _, path := range cfg.OpenAIPaths {
				if path == "" {
					continue
				}
				if r.bySync[path] == nil {
					r.bySync[path] = make(map[string]*Def)
				}
				r.bySync[path][cfg.Model] = def
			}
		}
	}
	return r
}

// RouteAsync returns the Def for the given (service_type, model) pair.
// If model is empty and only one model is configured for the type, it is used.
func (r *Registry) RouteAsync(serviceType, model string) (*Def, error) {
	models, ok := r.byTypeModel[serviceType]
	if !ok {
		return nil, fmt.Errorf("unknown service type %q", serviceType)
	}
	if model == "" {
		if len(models) == 1 {
			for _, d := range models {
				return d, nil
			}
		}
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("service type %q has multiple models, specify one via ?model= (available: %v)", serviceType, available)
	}
	d, ok := models[model]
	if !ok {
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("model %q not found for service type %q (available: %v)", model, serviceType, available)
	}
	return d, nil
}

// RouteSync returns the Def whose OpenAIPath and Model match the request,
// enabling model-field-based routing for OpenAI-compatible endpoints.
func (r *Registry) RouteSync(openaiPath, model string) (*Def, error) {
	models, ok := r.bySync[openaiPath]
	if !ok {
		return nil, fmt.Errorf("no sync service configured for path %q", openaiPath)
	}
	if d, ok := models[model]; ok {
		return d, nil
	}
	available := make([]string, 0, len(models))
	for m := range models {
		available = append(available, m)
	}
	return nil, fmt.Errorf("no service configured for path %q with model %q (available: %v)", openaiPath, model, available)
}

// HasSyncServices reports whether at least one service has sync mode configured.
func (r *Registry) HasSyncServices() bool {
	return len(r.bySync) > 0
}

// SyncPaths returns the unique OpenAI paths that have a sync backend configured.
func (r *Registry) SyncPaths() []string {
	paths := make([]string, 0, len(r.bySync))
	for p := range r.bySync {
		paths = append(paths, p)
	}
	return paths
}

// MaxFileSizeForType returns the largest MaxFileSizeMB across all models for
// the given service type. Used to set the body read limit before form parsing,
// before the specific model is known.
func (r *Registry) MaxFileSizeForType(serviceType string) (int64, error) {
	models, ok := r.byTypeModel[serviceType]
	if !ok {
		return 0, fmt.Errorf("unknown service type %q", serviceType)
	}
	var max int64
	for _, d := range models {
		if d.MaxFileSizeMB > max {
			max = d.MaxFileSizeMB
		}
	}
	return max, nil
}

// ValidateFileDef checks that the file's extension is accepted by the given service def.
func (r *Registry) ValidateFileDef(def *Def, filename string) error {
	if len(def.AcceptedExts) == 0 {
		return nil // no restriction configured
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if _, ok := def.AcceptedExts[ext]; !ok {
		accepted := make([]string, 0, len(def.AcceptedExts))
		for e := range def.AcceptedExts {
			accepted = append(accepted, e)
		}
		return fmt.Errorf("extension %q not accepted for model %q (accepted: %v)", ext, def.Model, accepted)
	}
	return nil
}

// Types returns all registered service type names (unique).
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.byTypeModel))
	for t := range r.byTypeModel {
		types = append(types, t)
	}
	return types
}

// Models returns all service definitions that expose an OpenAI-compatible model
// (i.e. have a non-empty Model field). Used for the GET /v1/models endpoint.
func (r *Registry) Models() []*Def {
	var defs []*Def
	for _, models := range r.byTypeModel {
		for _, d := range models {
			if d.Model != "" {
				defs = append(defs, d)
			}
		}
	}
	return defs
}

// All returns all service definitions (used to start result consumers).
func (r *Registry) All() []*Def {
	var defs []*Def
	for _, models := range r.byTypeModel {
		for _, d := range models {
			defs = append(defs, d)
		}
	}
	return defs
}

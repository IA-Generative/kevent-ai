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
	InferenceURL string              // full URL of the backend endpoint (direct proxy fallback)
	Operations   map[string][]string // operation name → URL paths (all indexed; first used for async)
	SyncTopic    string              // Kafka topic for priority sync-over-Kafka jobs (overrides direct proxy)
}

// OperationPath returns the first path for the given operation name.
// If op is empty and exactly one operation is configured, that operation is used.
func (d *Def) OperationPath(op string) (string, error) {
	if len(d.Operations) == 0 {
		return "", nil
	}
	if op == "" {
		if len(d.Operations) == 1 {
			for _, paths := range d.Operations {
				if len(paths) > 0 {
					return paths[0], nil
				}
			}
		}
		available := make([]string, 0, len(d.Operations))
		for name := range d.Operations {
			available = append(available, name)
		}
		return "", fmt.Errorf("model %q has multiple operations, specify one via -F operation=... (available: %v)", d.Model, available)
	}
	paths, ok := d.Operations[op]
	if !ok {
		available := make([]string, 0, len(d.Operations))
		for name := range d.Operations {
			available = append(available, name)
		}
		return "", fmt.Errorf("operation %q not found for model %q (available: %v)", op, d.Model, available)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("operation %q has no paths configured for model %q", op, d.Model)
	}
	return paths[0], nil
}

// pathPattern supports openai_paths entries that contain a {model} placeholder,
// e.g. "/v2/models/{model}/infer" or "/v1/models/{model}:predict".
// The placeholder can be an entire path segment or embedded within one
// (prefix/suffix around {model} in the same segment are matched literally).
type pathPattern struct {
	pattern     string
	segments    []string        // pattern split by "/"
	defsByModel map[string]*Def // model name → Def
}

// match returns the extracted model name if urlPath matches the pattern.
func (p *pathPattern) match(urlPath string) (model string, ok bool) {
	urlSegs := strings.Split(urlPath, "/")
	if len(urlSegs) != len(p.segments) {
		return "", false
	}
	for i, patSeg := range p.segments {
		if strings.Contains(patSeg, "{model}") {
			m, matched := matchModelSegment(patSeg, urlSegs[i])
			if !matched || m == "" {
				return "", false
			}
			model = m
		} else if urlSegs[i] != patSeg {
			return "", false
		}
	}
	return model, model != ""
}

// matchModelSegment extracts the model name from a URL segment given a pattern
// segment that contains exactly one {model} placeholder.
// Examples:
//
//	pattern "{model}"         + actual "whisper-large-v3"         → "whisper-large-v3"
//	pattern "{model}:predict" + actual "whisper-large-v3:predict" → "whisper-large-v3"
func matchModelSegment(pattern, actual string) (model string, ok bool) {
	parts := strings.SplitN(pattern, "{model}", 2)
	prefix, suffix := parts[0], parts[1]
	if !strings.HasPrefix(actual, prefix) || !strings.HasSuffix(actual, suffix) {
		return "", false
	}
	// Guard against overlap when prefix+suffix is longer than actual.
	if len(actual) < len(prefix)+len(suffix) {
		return "", false
	}
	return actual[len(prefix) : len(actual)-len(suffix)], true
}

// Registry maps (service_type, model) pairs to their runtime definitions.
type Registry struct {
	byTypeModel   map[string]map[string]*Def // type → model → Def
	defaultByType map[string]*Def            // type → default Def (when default: true in config)
	bySync        map[string]map[string]*Def // exact openai_path → model → Def
	defaultByPath map[string]*Def            // exact openai_path → default Def
	byPattern     []*pathPattern             // pattern paths containing {model}
}

func NewRegistry(cfgs []config.ServiceConfig) *Registry {
	r := &Registry{
		byTypeModel:   make(map[string]map[string]*Def, len(cfgs)),
		defaultByType: make(map[string]*Def),
		bySync:        make(map[string]map[string]*Def),
		defaultByPath: make(map[string]*Def),
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
			Operations:    cfg.Operations,
			SyncTopic:     cfg.SyncTopic,
		}

		if r.byTypeModel[cfg.Type] == nil {
			r.byTypeModel[cfg.Type] = make(map[string]*Def)
		}
		r.byTypeModel[cfg.Type][cfg.Model] = def

		if cfg.Default {
			r.defaultByType[cfg.Type] = def
		}

		// Build the sync routing index — one entry per configured path across all operations.
		// Index when either a direct proxy URL or a sync Kafka topic is configured.
		if cfg.Model != "" && (cfg.InferenceURL != "" || cfg.SyncTopic != "") {
			for _, paths := range cfg.Operations {
				for _, path := range paths {
					if path == "" {
						continue
					}
					if strings.Contains(path, "{model}") {
						// Pattern path — model is embedded in the URL.
						r.indexPattern(path, cfg.Model, def)
					} else {
						// Exact path — model is expected in the request body.
						if r.bySync[path] == nil {
							r.bySync[path] = make(map[string]*Def)
						}
						r.bySync[path][cfg.Model] = def
						if cfg.Default {
							r.defaultByPath[path] = def
						}
					}
				}
			}
		}
	}
	return r
}

// indexPattern adds def to the pattern index, merging into an existing pattern
// entry when the same path template is shared by multiple service configs.
func (r *Registry) indexPattern(pattern, model string, def *Def) {
	for _, p := range r.byPattern {
		if p.pattern == pattern {
			p.defsByModel[model] = def
			return
		}
	}
	r.byPattern = append(r.byPattern, &pathPattern{
		pattern:     pattern,
		segments:    strings.Split(pattern, "/"),
		defsByModel: map[string]*Def{model: def},
	})
}

// RouteAsync returns the Def for the given (service_type, model) pair.
// Resolution order when model is empty:
//  1. Single model configured for the type → auto-selected.
//  2. A model marked default: true for the type → used as fallback.
//  3. Error listing available models.
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
		if d, ok := r.defaultByType[serviceType]; ok {
			return d, nil
		}
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("service type %q has multiple models, specify one via -F model=... (available: %v)", serviceType, available)
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

// RouteSync returns the service Def for the incoming request.
//
// Lookup order:
//  1. Exact path match — model from request body.
//     If model is empty: single registered model, then default model, then error.
//  2. Pattern path match — model extracted from URL.
func (r *Registry) RouteSync(openaiPath, model string) (*Def, error) {
	// 1. Exact path.
	if models, ok := r.bySync[openaiPath]; ok {
		if d, ok := models[model]; ok {
			return d, nil
		}
		if model == "" {
			if len(models) == 1 {
				for _, d := range models {
					return d, nil
				}
			}
			if d, ok := r.defaultByPath[openaiPath]; ok {
				return d, nil
			}
		}
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("no service for path %q with model %q (available: %v)", openaiPath, model, available)
	}

	// 2. Pattern path — model extracted from URL.
	for _, p := range r.byPattern {
		extracted, ok := p.match(openaiPath)
		if !ok {
			continue
		}
		if d, ok := p.defsByModel[extracted]; ok {
			return d, nil
		}
		available := make([]string, 0, len(p.defsByModel))
		for m := range p.defsByModel {
			available = append(available, m)
		}
		return nil, fmt.Errorf("path %q matched pattern %q but model %q is not registered (available: %v)",
			openaiPath, p.pattern, extracted, available)
	}

	return nil, fmt.Errorf("no sync service configured for path %q", openaiPath)
}

// HasSyncServices reports whether at least one service has sync mode configured.
func (r *Registry) HasSyncServices() bool {
	return len(r.bySync) > 0 || len(r.byPattern) > 0
}

// SyncPaths returns the unique OpenAI paths/patterns that have a sync backend.
func (r *Registry) SyncPaths() []string {
	paths := make([]string, 0, len(r.bySync)+len(r.byPattern))
	for p := range r.bySync {
		paths = append(paths, p)
	}
	for _, p := range r.byPattern {
		paths = append(paths, p.pattern)
	}
	return paths
}

// SyncPathPrefixes returns unique first-level path prefixes (e.g. "/v1", "/v2")
// across all registered sync paths. Used to register chi wildcard routes.
func (r *Registry) SyncPathPrefixes() []string {
	seen := make(map[string]struct{})
	for path := range r.bySync {
		seen[pathPrefix(path)] = struct{}{}
	}
	for _, p := range r.byPattern {
		seen[pathPrefix(p.pattern)] = struct{}{}
	}
	prefixes := make([]string, 0, len(seen))
	for prefix := range seen {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// pathPrefix returns the first non-empty path segment with a leading slash,
// e.g. "/v1/audio/transcriptions" → "/v1", "/v2/models/{model}/infer" → "/v2".
func pathPrefix(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	return "/" + parts[0]
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

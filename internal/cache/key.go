package cache

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// cacheableFields is the set of request fields included in the cache key.
// Unknown fields (user, metadata, etc.) are excluded to avoid cardinality explosion.
var cacheableFields = map[string]bool{
	"model": true, "messages": true, "temperature": true, "top_p": true,
	"top_k": true, "max_tokens": true, "max_completion_tokens": true,
	"response_format": true, "tools": true, "tool_choice": true,
	"seed": true, "stop": true, "presence_penalty": true,
	"frequency_penalty": true, "n": true, "logit_bias": true, "system": true,
}

// Key derives a Redis key for the given provider, canonical model name, and
// raw JSON request body. Returns ("", false, nil) when the request is not
// cacheable (e.g. stream=true or unparseable body).
func Key(provider, model string, body []byte) (string, bool, error) {
	if len(body) == 0 {
		return "", false, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", false, fmt.Errorf("cache key: unmarshal body: %w", err)
	}

	// Streaming responses are not cached.
	if s, ok := raw["stream"]; ok {
		switch v := s.(type) {
		case bool:
			if v {
				return "", false, nil
			}
		}
	}

	// Use the canonical model name from the registry, not what the client sent.
	raw["model"] = model

	// Keep only cacheable fields.
	filtered := make(map[string]any, len(cacheableFields))
	for k, v := range raw {
		if cacheableFields[k] {
			filtered[k] = v
		}
	}

	canonical, err := canonicalize(filtered)
	if err != nil {
		return "", false, fmt.Errorf("cache key: canonicalize: %w", err)
	}

	h := sha256.Sum256([]byte(provider + ":" + canonical))
	return fmt.Sprintf("llm:cache:%x", h), true, nil
}

// canonicalize produces a deterministic JSON string by sorting object keys recursively.
func canonicalize(v any) (string, error) {
	sorted, err := sortedValue(v)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sortedValue(v any) (any, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		type kv struct {
			K string
			V any
		}
		pairs := make([]kv, 0, len(keys))
		for _, k := range keys {
			sv, err := sortedValue(val[k])
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, kv{K: k, V: sv})
		}
		// Re-encode as a sorted map via a slice trick: marshal as object with sorted keys.
		// We rebuild a map; json.Marshal iterates maps in arbitrary order in older Go, but
		// since Go 1.12 it sorts map keys. This is reliable.
		out := make(map[string]any, len(pairs))
		for _, p := range pairs {
			out[p.K] = p.V
		}
		return out, nil
	case []any:
		result := make([]any, len(val))
		for i, item := range val {
			sv, err := sortedValue(item)
			if err != nil {
				return nil, err
			}
			result[i] = sv
		}
		return result, nil
	default:
		return v, nil
	}
}

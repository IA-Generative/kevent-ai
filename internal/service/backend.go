package service

import (
	"math/rand/v2"
	"sort"
)

// Backend is a resolved inference backend with its routing weight.
type Backend struct {
	URL     string
	Weight  int               // 0 = fallback-only (never primary-selected)
	Headers map[string]string // per-backend headers; override service-level InferenceHeaders
}

// OrderedBackends returns the backends in the order they should be tried for a
// single request:
//  1. One backend chosen by weighted-random among weight>0 backends (primary).
//  2. Remaining weight>0 backends sorted by descending weight (secondary fallbacks).
//  3. weight=0 backends in their original config order (last-resort fallbacks).
//
// If all backends have weight=0 they are returned in config order.
// Returns an empty slice for empty input.
func OrderedBackends(backends []Backend) []Backend {
	if len(backends) == 0 {
		return nil
	}

	var positives, zeroes []Backend
	for _, b := range backends {
		if b.Weight > 0 {
			positives = append(positives, b)
		} else {
			zeroes = append(zeroes, b)
		}
	}

	if len(positives) == 0 {
		out := make([]Backend, len(backends))
		copy(out, backends)
		return out
	}

	// Weighted-random primary selection.
	total := 0
	for _, b := range positives {
		total += b.Weight
	}
	r := rand.IntN(total)
	cumulative := 0
	primaryIdx := 0
	for i, b := range positives {
		cumulative += b.Weight
		if r < cumulative {
			primaryIdx = i
			break
		}
	}

	primary := positives[primaryIdx]
	rest := make([]Backend, 0, len(positives)-1)
	rest = append(rest, positives[:primaryIdx]...)
	rest = append(rest, positives[primaryIdx+1:]...)
	sort.SliceStable(rest, func(i, j int) bool {
		return rest[i].Weight > rest[j].Weight
	})

	out := make([]Backend, 0, len(backends))
	out = append(out, primary)
	out = append(out, rest...)
	out = append(out, zeroes...)
	return out
}

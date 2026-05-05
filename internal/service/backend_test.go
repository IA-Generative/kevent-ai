package service

import (
	"math"
	"testing"
)

func TestOrderedBackends_Empty(t *testing.T) {
	if got := OrderedBackends(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if got := OrderedBackends([]Backend{}); got != nil {
		t.Fatalf("expected nil for empty slice, got %v", got)
	}
}

func TestOrderedBackends_SingleBackend(t *testing.T) {
	in := []Backend{{URL: "http://a", Weight: 1}}
	got := OrderedBackends(in)
	if len(got) != 1 || got[0].URL != "http://a" {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestOrderedBackends_AllWeightZero(t *testing.T) {
	in := []Backend{
		{URL: "http://a", Weight: 0},
		{URL: "http://b", Weight: 0},
	}
	got := OrderedBackends(in)
	if len(got) != 2 || got[0].URL != "http://a" || got[1].URL != "http://b" {
		t.Fatalf("expected config order for all-zero weights, got %v", got)
	}
}

func TestOrderedBackends_ZeroWeightAlwaysLast(t *testing.T) {
	in := []Backend{
		{URL: "http://fallback", Weight: 0},
		{URL: "http://primary", Weight: 100},
	}
	for i := range 200 {
		got := OrderedBackends(in)
		if got[len(got)-1].URL != "http://fallback" {
			t.Fatalf("iteration %d: weight=0 backend not last: %v", i, got)
		}
	}
}

func TestOrderedBackends_SecondariesDescending(t *testing.T) {
	in := []Backend{
		{URL: "http://low", Weight: 10},
		{URL: "http://high", Weight: 80},
		{URL: "http://mid", Weight: 40},
	}
	// Run many times to exercise all primary selections.
	for i := range 500 {
		got := OrderedBackends(in)
		if len(got) != 3 {
			t.Fatalf("expected 3 backends, got %d", len(got))
		}
		// After primary, remaining should be in descending weight order.
		primary := got[0]
		rest := got[1:]
		for j := 1; j < len(rest); j++ {
			if rest[j].Weight > rest[j-1].Weight {
				t.Fatalf("iteration %d: secondaries not descending after primary=%s: %v", i, primary.URL, rest)
			}
		}
	}
}

func TestOrderedBackends_WeightedDistribution(t *testing.T) {
	in := []Backend{
		{URL: "http://a", Weight: 90},
		{URL: "http://b", Weight: 10},
	}
	const iterations = 20_000
	counts := map[string]int{}
	for range iterations {
		got := OrderedBackends(in)
		counts[got[0].URL]++
	}
	// Expect ~90% for "a" and ~10% for "b". Allow ±3% tolerance.
	pctA := float64(counts["http://a"]) / iterations
	if math.Abs(pctA-0.90) > 0.03 {
		t.Errorf("expected ~90%% for http://a, got %.1f%%", pctA*100)
	}
}

func TestOrderedBackends_AllBackendsPresent(t *testing.T) {
	in := []Backend{
		{URL: "http://a", Weight: 50},
		{URL: "http://b", Weight: 50},
		{URL: "http://c", Weight: 0},
	}
	got := OrderedBackends(in)
	if len(got) != 3 {
		t.Fatalf("expected all 3 backends, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, b := range got {
		seen[b.URL] = true
	}
	for _, b := range in {
		if !seen[b.URL] {
			t.Errorf("backend %s missing from result", b.URL)
		}
	}
}

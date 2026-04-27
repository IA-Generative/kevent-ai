package cache

import (
	"strings"
	"testing"
)

func TestKey_EmptyBody(t *testing.T) {
	key, cacheable, err := Key("openai", "gpt-4o", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cacheable {
		t.Error("empty body should not be cacheable")
	}
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
}

func TestKey_StreamTrue(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	key, cacheable, err := Key("openai", "gpt-4o", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cacheable {
		t.Error("stream=true should not be cacheable")
	}
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
}

func TestKey_StreamFalse_IsCacheable(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	key, cacheable, err := Key("openai", "gpt-4o", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cacheable {
		t.Error("stream=false should be cacheable")
	}
	if key == "" {
		t.Error("expected non-empty key")
	}
}

func TestKey_InvalidJSON(t *testing.T) {
	_, _, err := Key("openai", "gpt-4o", []byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestKey_DifferentUnknownFields_SameKey(t *testing.T) {
	// "user" and "metadata" are not in the cacheable set — ignored in key.
	a := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"user":"alice"}`)
	b := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"user":"bob","metadata":{"foo":"bar"}}`)

	keyA, _, err := Key("openai", "gpt-4o", a)
	if err != nil {
		t.Fatal(err)
	}
	keyB, _, err := Key("openai", "gpt-4o", b)
	if err != nil {
		t.Fatal(err)
	}
	if keyA != keyB {
		t.Errorf("expected same key for requests differing only in non-cacheable fields; got %q vs %q", keyA, keyB)
	}
}

func TestKey_DifferentMessages_DifferentKey(t *testing.T) {
	a := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	b := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"world"}]}`)

	keyA, _, _ := Key("openai", "gpt-4o", a)
	keyB, _, _ := Key("openai", "gpt-4o", b)
	if keyA == keyB {
		t.Error("different messages should produce different keys")
	}
}

func TestKey_FieldOrderIrrelevant(t *testing.T) {
	// Same logical request, different JSON field ordering.
	a := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o","temperature":0.7}`)
	b := []byte(`{"temperature":0.7,"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	keyA, _, err := Key("openai", "gpt-4o", a)
	if err != nil {
		t.Fatal(err)
	}
	keyB, _, err := Key("openai", "gpt-4o", b)
	if err != nil {
		t.Fatal(err)
	}
	if keyA != keyB {
		t.Errorf("field order should not affect cache key; got %q vs %q", keyA, keyB)
	}
}

func TestKey_DifferentProviders_DifferentKey(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	keyOAI, _, _ := Key("openai", "gpt-4o", body)
	keyAnt, _, _ := Key("anthropic", "gpt-4o", body)
	if keyOAI == keyAnt {
		t.Error("different providers should produce different keys")
	}
}

func TestKey_CanonicalModelOverridesBody(t *testing.T) {
	// Client sends "GPT-4o" but registry model is "gpt-4o" — key uses registry value.
	a := []byte(`{"model":"GPT-4o","messages":[{"role":"user","content":"hi"}]}`)
	b := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	// Both should produce the same key because Key() overwrites model with the canonical name.
	keyA, _, _ := Key("openai", "gpt-4o", a)
	keyB, _, _ := Key("openai", "gpt-4o", b)
	if keyA != keyB {
		t.Errorf("canonical model override should normalise client model field; got %q vs %q", keyA, keyB)
	}
}

func TestKey_Prefix(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	key, cacheable, err := Key("openai", "gpt-4o", body)
	if err != nil || !cacheable {
		t.Fatalf("unexpected: err=%v cacheable=%v", err, cacheable)
	}
	if !strings.HasPrefix(key, "llm:cache:") {
		t.Errorf("key should start with 'llm:cache:'; got %q", key)
	}
}

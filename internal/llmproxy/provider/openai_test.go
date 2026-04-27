package provider

import "testing"

func TestParseOpenAIUsage_Valid(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":15,"completion_tokens":42}}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.PromptTokens != 15 {
		t.Errorf("expected prompt_tokens=15, got %d", u.PromptTokens)
	}
	if u.CompletionTokens != 42 {
		t.Errorf("expected completion_tokens=42, got %d", u.CompletionTokens)
	}
}

func TestParseOpenAIUsage_MissingUsage(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-123","choices":[]}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage even when usage field missing")
	}
	if u.PromptTokens != 0 || u.CompletionTokens != 0 {
		t.Errorf("expected zero tokens, got prompt=%d completion=%d", u.PromptTokens, u.CompletionTokens)
	}
}

func TestParseOpenAIUsage_InvalidJSON(t *testing.T) {
	u := parseOpenAIUsage([]byte(`not json`))
	if u != nil {
		t.Error("expected nil for invalid JSON")
	}
}

package provider

import (
	"encoding/json"
	"testing"
)

// ── openAIToAnthropic ────────────────────────────────────────────────────────

func TestOpenAIToAnthropic_BasicUserMessage(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [{"role": "user", "content": "Hello"}],
		"max_tokens": 100
	}`)

	out, err := openAIToAnthropic(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	msgs := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("expected role=user, got %q", msg["role"])
	}
	if msg["content"] != "Hello" {
		t.Errorf("expected content=Hello, got %q", msg["content"])
	}
	if _, ok := req["system"]; ok {
		t.Error("system field should be absent when no system message present")
	}
}

func TestOpenAIToAnthropic_SystemMessageExtracted(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hi"}
		]
	}`)

	out, err := openAIToAnthropic(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if req["system"] != "You are helpful." {
		t.Errorf("expected system=%q, got %q", "You are helpful.", req["system"])
	}

	msgs := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 non-system message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("expected role=user, got %q", msg["role"])
	}
}

func TestOpenAIToAnthropic_MultipleSystemMessagesConcatenated(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{"role": "system", "content": "Part one."},
			{"role": "system", "content": "Part two."},
			{"role": "user", "content": "Hello"}
		]
	}`)

	out, err := openAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}

	var req map[string]any
	json.Unmarshal(out, &req)
	system := req["system"].(string)
	if system != "Part one.\nPart two." {
		t.Errorf("expected concatenated system, got %q", system)
	}
}

func TestOpenAIToAnthropic_DefaultMaxTokens(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`)

	out, _ := openAIToAnthropic(body)
	var req map[string]any
	json.Unmarshal(out, &req)

	maxTok := int(req["max_tokens"].(float64))
	if maxTok != 4096 {
		t.Errorf("expected default max_tokens=4096, got %d", maxTok)
	}
}

func TestOpenAIToAnthropic_StopStringConvertedToSlice(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [{"role": "user", "content": "Hi"}],
		"stop": "DONE"
	}`)

	out, _ := openAIToAnthropic(body)
	var req map[string]any
	json.Unmarshal(out, &req)

	seqs, ok := req["stop_sequences"].([]any)
	if !ok || len(seqs) != 1 || seqs[0] != "DONE" {
		t.Errorf("expected stop_sequences=[DONE], got %v", req["stop_sequences"])
	}
}

func TestOpenAIToAnthropic_StopArrayPreserved(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [{"role": "user", "content": "Hi"}],
		"stop": ["END", "STOP"]
	}`)

	out, _ := openAIToAnthropic(body)
	var req map[string]any
	json.Unmarshal(out, &req)

	seqs, ok := req["stop_sequences"].([]any)
	if !ok || len(seqs) != 2 {
		t.Errorf("expected 2 stop_sequences, got %v", req["stop_sequences"])
	}
}

// ── anthropicToOpenAI ────────────────────────────────────────────────────────

func TestAnthropicToOpenAI_BasicResponse(t *testing.T) {
	anthropicBody := []byte(`{
		"id": "msg_01",
		"model": "claude-sonnet-4-5-20251001",
		"type": "message",
		"content": [{"type": "text", "text": "Hello there!"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)

	out, usage, err := anthropicToOpenAI(anthropicBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if resp["object"] != "chat.completion" {
		t.Errorf("expected object=chat.completion, got %q", resp["object"])
	}

	choices := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason=stop, got %q", choice["finish_reason"])
	}

	message := choice["message"].(map[string]any)
	if message["content"] != "Hello there!" {
		t.Errorf("expected content=%q, got %q", "Hello there!", message["content"])
	}

	if usage == nil {
		t.Fatal("expected usage to be non-nil")
	}
	if usage.PromptTokens != 10 {
		t.Errorf("expected prompt_tokens=10, got %d", usage.PromptTokens)
	}
	if usage.CompletionTokens != 5 {
		t.Errorf("expected completion_tokens=5, got %d", usage.CompletionTokens)
	}
}

func TestAnthropicToOpenAI_UsageMappedToOpenAIShape(t *testing.T) {
	body := []byte(`{
		"id": "msg_02",
		"model": "claude-sonnet-4-5-20251001",
		"content": [{"type": "text", "text": "OK"}],
		"stop_reason": "max_tokens",
		"usage": {"input_tokens": 20, "output_tokens": 30}
	}`)

	out, _, _ := anthropicToOpenAI(body)
	var resp map[string]any
	json.Unmarshal(out, &resp)

	u := resp["usage"].(map[string]any)
	if int(u["prompt_tokens"].(float64)) != 20 {
		t.Errorf("expected prompt_tokens=20, got %v", u["prompt_tokens"])
	}
	if int(u["completion_tokens"].(float64)) != 30 {
		t.Errorf("expected completion_tokens=30, got %v", u["completion_tokens"])
	}
	if int(u["total_tokens"].(float64)) != 50 {
		t.Errorf("expected total_tokens=50, got %v", u["total_tokens"])
	}
}

// ── anthropicFinishReason ────────────────────────────────────────────────────

func TestAnthropicFinishReason(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
		{"unknown_reason", "unknown_reason"}, // passthrough
	}
	for _, tc := range cases {
		got := anthropicFinishReason(tc.in)
		if got != tc.want {
			t.Errorf("anthropicFinishReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── contentToString ──────────────────────────────────────────────────────────

func TestContentToString_String(t *testing.T) {
	if got := contentToString("hello"); got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestContentToString_PartsArray(t *testing.T) {
	parts := []any{
		map[string]any{"type": "text", "text": "foo"},
		map[string]any{"type": "text", "text": "bar"},
	}
	if got := contentToString(parts); got != "foobar" {
		t.Errorf("got %q", got)
	}
}

func TestContentToString_Unknown(t *testing.T) {
	if got := contentToString(42); got != "" {
		t.Errorf("expected empty string for unknown type, got %q", got)
	}
}

// Package guardrails provides PII detection for LLM request payloads.
// Patterns cover common French/EU personally identifiable information.
// False-positive rates for SIREN/SIRET are higher than other patterns —
// enable per-service after assessing your payload characteristics.
package guardrails

import (
	"encoding/json"
	"regexp"
)

type namedPattern struct {
	name string
	re   *regexp.Regexp
}

var compiledPatterns = []namedPattern{
	{
		"email",
		regexp.MustCompile(`(?i)[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
	},
	{
		"phone_fr",
		// +33, 0033, or 0 followed by a non-zero digit, groups of 2.
		regexp.MustCompile(`(?:(?:\+|00)33|0)\s*[1-9](?:[\s.\-]*\d{2}){4}`),
	},
	{
		"iban",
		regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}[A-Z0-9]{0,16}\b`),
	},
	{
		"credit_card",
		// 13–16 digits, optionally space- or dash-separated.
		regexp.MustCompile(`\b(?:\d[ \-]?){13,16}\b`),
	},
	{
		"siret",
		// 14-digit SIRET checked before 9-digit SIREN to avoid double reporting.
		regexp.MustCompile(`\b\d{14}\b`),
	},
	{
		"siren",
		regexp.MustCompile(`\b\d{9}\b`),
	},
}

// Checker scans OpenAI-compatible JSON message payloads for PII patterns.
// A single Checker instance is safe for concurrent use.
type Checker struct{}

// New returns a ready-to-use Checker.
func New() *Checker { return &Checker{} }

// Check scans the "content" fields of messages in an OpenAI-compatible
// JSON payload body. Returns the names of PII categories detected.
// Returns nil when no violations are found or the body is not a message payload.
func (c *Checker) Check(body []byte) []string {
	texts := extractMessageTexts(body)
	if len(texts) == 0 {
		return nil
	}

	// Track which categories have already been reported to avoid duplicates.
	found := make(map[string]struct{})
	var violations []string
	for _, text := range texts {
		for _, p := range compiledPatterns {
			if _, seen := found[p.name]; seen {
				continue
			}
			if p.re.MatchString(text) {
				found[p.name] = struct{}{}
				violations = append(violations, p.name)
			}
		}
	}
	return violations
}

// extractMessageTexts pulls plaintext from the "messages[*].content" field
// of an OpenAI-compatible payload. Content may be a string or an array of
// content parts (each with a "text" field).
func extractMessageTexts(body []byte) []string {
	var payload struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		return nil
	}

	var texts []string
	for _, msg := range payload.Messages {
		if len(msg.Content) == 0 {
			continue
		}
		// Try string content first (most common).
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			texts = append(texts, s)
			continue
		}
		// Try array of content parts (vision/multimodal payloads).
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err == nil {
			for _, p := range parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts
}

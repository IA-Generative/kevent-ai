package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"kevent/dispatcher/internal/config"
)

type ocrAdapter struct {
	cfg    config.OCRConfig
	inf    config.InferenceConfig
	client *http.Client
}

func newOCR(cfg config.OCRConfig, inf config.InferenceConfig) *ocrAdapter {
	return &ocrAdapter{
		cfg:    cfg,
		inf:    inf,
		client: newHTTPClient("", inf),
	}
}

// Call reads the image file into memory, base64-encodes it, and sends it to
// the OCR endpoint using the OpenAI vision chat completions format.
func (a *ocrAdapter) Call(ctx context.Context, input CallInput) ([]byte, error) {
	endpointURL := buildURL(a.inf, input)
	if endpointURL == "" {
		return nil, fmt.Errorf("ocr: cannot build endpoint URL (inference.base_url or InferenceURL missing)")
	}

	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	dataURL := fmt.Sprintf("data:%s;base64,%s", input.ContentType, encoded)

	reqBody := map[string]any{
		"model": input.Model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
					{"type": "text", "text": a.cfg.Prompt},
				},
			},
		},
		"max_tokens": a.cfg.MaxTokens,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling OCR request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.inf.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.inf.APIKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling OCR endpoint: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OCR endpoint returned %d: %s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

package adapter

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"kevent/dispatcher/internal/config"
)

type diarizationAdapter struct {
	cfg    config.DiarizationConfig
	inf    config.InferenceConfig
	client *http.Client
}

func newDiarization(cfg config.DiarizationConfig, inf config.InferenceConfig) *diarizationAdapter {
	return &diarizationAdapter{
		cfg:    cfg,
		inf:    inf,
		client: newHTTPClient("", inf),
	}
}

// Call streams the audio file to the diarization endpoint via multipart form.
// An io.Pipe avoids loading the file (up to 500 MB) into memory.
func (a *diarizationAdapter) Call(ctx context.Context, input CallInput) ([]byte, error) {
	endpointURL := buildURL(a.inf, input)
	if endpointURL == "" {
		return nil, fmt.Errorf("diarization: cannot build endpoint URL (inference.base_url or InferenceURL missing)")
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			if err := mw.WriteField("model", input.Model); err != nil {
				return err
			}
			if a.cfg.NumSpeakers > 0 {
				if err := mw.WriteField("num_speakers", strconv.Itoa(a.cfg.NumSpeakers)); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("file", input.Filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, input.Body); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err) // nil → io.EOF (normal close)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, pr)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if a.inf.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.inf.APIKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling diarization endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("diarization endpoint returned %d: %s", resp.StatusCode, body)
	}

	return body, nil
}

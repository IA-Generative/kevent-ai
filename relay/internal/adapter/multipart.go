package adapter

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"

	"kevent/relay/internal/config"
)

// multipartAdapter streams a file to any multipart/form-data inference endpoint.
// It sends a "model" field (from the event) and any extra_fields from config,
// then attaches the file as "file". Field order is stable (sorted keys).
type multipartAdapter struct {
	inf    config.InferenceConfig
	client *http.Client
}

func (a *multipartAdapter) Call(ctx context.Context, input CallInput) ([]byte, error) {
	url := endpointURL(a.inf, input)
	if url == "" {
		return nil, fmt.Errorf("cannot build endpoint URL: inference.base_url or InferenceURL is empty")
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			if input.Model != "" {
				if err := mw.WriteField("model", input.Model); err != nil {
					return err
				}
			}
			// Write extra_fields in deterministic order.
			keys := make([]string, 0, len(a.inf.ExtraFields))
			for k := range a.inf.ExtraFields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v := a.inf.ExtraFields[k]; v != "" {
					if err := mw.WriteField(k, v); err != nil {
						return err
					}
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
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if a.inf.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.inf.APIKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling inference endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inference endpoint returned %d: %s", resp.StatusCode, body)
	}

	return body, nil
}

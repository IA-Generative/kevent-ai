package asyncworker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
)

// callInput holds everything needed to build a multipart inference request.
type callInput struct {
	url         string
	apiKey      string
	filename    string
	body        []byte
	contentType string
	model       string
	params      map[string]string // per-job fields (from Job.Params)
	extraFields map[string]string // static service-level fields (from ServiceConfig)
}

// callInference posts a multipart request to the inference backend and returns the response body.
// Field order: model → merged extra_fields+params (sorted) → file.
func callInference(ctx context.Context, client *http.Client, in callInput) ([]byte, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			if in.model != "" {
				if err := mw.WriteField("model", in.model); err != nil {
					return err
				}
			}
			// Merge static extra_fields with per-job params; params take precedence.
			merged := make(map[string]string, len(in.extraFields)+len(in.params))
			for k, v := range in.extraFields {
				merged[k] = v
			}
			for k, v := range in.params {
				merged[k] = v
			}
			keys := make([]string, 0, len(merged))
			for k := range merged {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v := merged[k]; v != "" {
					if err := mw.WriteField(k, v); err != nil {
						return err
					}
				}
			}
			filename := in.filename
			if filename == "" {
				filename = "file"
			}
			part, err := mw.CreateFormFile("file", filepath.Base(filename))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, bytes.NewReader(in.body)); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.url, pr)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if in.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+in.apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling inference: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inference returned %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"kevent/gateway/internal/config"
)

// SwaggerSpec holds a fetched OpenAPI spec for one service.
type SwaggerSpec struct {
	Type  string
	Model string
	Data  json.RawMessage
}

// key returns the lookup key used to index and serve the spec.
func (s SwaggerSpec) key() string { return s.Type + "/" + s.Model }

// FetchSwaggerSpecs fetches OpenAPI specs from each service's swagger_url field.
// Failures are logged as warnings and skipped — they never block startup.
func FetchSwaggerSpecs(cfgs []config.ServiceConfig) []SwaggerSpec {
	client := &http.Client{Timeout: 10 * time.Second}
	var specs []SwaggerSpec
	for _, svc := range cfgs {
		if svc.SwaggerURL == "" {
			continue
		}
		data, err := fetchSwaggerJSON(client, svc.SwaggerURL, svc.SwaggerHeaders)
		if err != nil {
			slog.Warn("failed to fetch swagger spec",
				"type", svc.Type, "model", svc.Model,
				"url", svc.SwaggerURL, "error", err)
			continue
		}
		specs = append(specs, SwaggerSpec{Type: svc.Type, Model: svc.Model, Data: data})
		slog.Info("swagger spec loaded", "type", svc.Type, "model", svc.Model)
	}
	return specs
}

func fetchSwaggerJSON(client *http.Client, url string, headers map[string]string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB limit
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("response is not valid JSON")
	}
	return json.RawMessage(data), nil
}

// NewSwaggerHandler returns a handler that serves cached service OpenAPI specs.
// Route: GET /swagger/{type}/{model}
func NewSwaggerHandler(specs []SwaggerSpec) http.HandlerFunc {
	index := make(map[string]json.RawMessage, len(specs))
	for _, s := range specs {
		index[s.key()] = s.Data
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "type") + "/" + chi.URLParam(r, "model")
		data, ok := index[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

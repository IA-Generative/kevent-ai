package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"kevent/gateway/internal/service"
)

// GenerateSpec builds an OpenAPI 3.0.3 spec dynamically from the service registry.
// Call once at startup after the registry is initialised; serve the result statically.
func GenerateSpec(reg *service.Registry, appVersion string) []byte {
	paths := map[string]any{}

	// ── Fixed endpoints ────────────────────────────────────────────────────────
	paths["/health"] = healthPathItem()

	serviceTypes := reg.Types()
	sort.Strings(serviceTypes)

	paths["/jobs/{service_type}"] = submitJobPathItem(serviceTypes, reg)
	paths["/jobs/{service_type}/{id}"] = getJobPathItem(serviceTypes)

	if reg.HasSyncServices() {
		paths["/v1/models"] = listModelsPathItem()
		for p, item := range syncPathItems(reg) {
			paths[p] = item
		}
	}

	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Kevent Inference Gateway",
			"version": appVersion,
			"description": "Gateway for asynchronous and synchronous inference jobs.\n\n" +
				"**Async mode** — submit a file, get a `job_id`, poll for result or receive a webhook.\n\n" +
				"**Sync mode** — OpenAI-compatible endpoints, response held open until inference is done.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"tags": []any{
			map[string]any{"name": "Jobs", "description": "Async job submission and status"},
			map[string]any{"name": "Inference", "description": "Synchronous OpenAI-compatible inference endpoints"},
		},
		"paths":      paths,
		"components": specComponents(),
	}

	out, _ := yaml.Marshal(spec)
	return out
}

// NewDocsSpec returns a handler that serves the pre-generated OpenAPI spec.
func NewDocsSpec(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	}
}

// DocsUI serves the Swagger UI at GET /docs.
// specs is the list of pre-fetched service OpenAPI specs; they appear as additional
// entries in the Swagger UI dropdown alongside the gateway's own spec.
func DocsUI(specs []SwaggerSpec) http.HandlerFunc {
	// Build the Swagger UI urls array. Gateway spec is always first (primary).
	type urlEntry struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	entries := []urlEntry{
		{Name: "Gateway (jobs async + sync)", URL: "/openapi.yaml"},
	}
	// Group by type for readable names: "audio / whisper-large-v3"
	for _, s := range specs {
		name := s.Type + " / " + s.Model
		entries = append(entries, urlEntry{Name: name, URL: "/swagger/" + s.Type + "/" + s.Model})
	}
	urlsJSON, _ := json.Marshal(entries)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kevent API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui.css" />
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui-standalone-preset.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({
      urls: ` + string(urlsJSON) + `,
      "urls.primaryName": "Gateway (jobs async + sync)",
      dom_id: "#swagger-ui",
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
      layout: "StandaloneLayout",
      deepLinking: true,
    });
  </script>
</body>
</html>`

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}
}

// ── Path item builders ─────────────────────────────────────────────────────────

func healthPathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"summary":     "Health check",
			"operationId": "healthCheck",
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Service is healthy",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"status": map[string]any{"type": "string", "example": "ok"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func submitJobPathItem(serviceTypes []string, reg *service.Registry) map[string]any {
	// Collect models and operations per service type.
	modelsByType := map[string][]string{}
	allOps := []string{}
	for _, def := range reg.All() {
		if def.Model != "" {
			modelsByType[def.Type] = uniqueSorted(append(modelsByType[def.Type], def.Model))
		}
		for opName := range def.Operations {
			allOps = appendUniq(allOps, opName)
		}
	}
	sort.Strings(allOps)

	// Build a readable description of models per type.
	typeParts := make([]string, 0, len(serviceTypes))
	for _, t := range serviceTypes {
		if models := modelsByType[t]; len(models) > 0 {
			typeParts = append(typeParts, fmt.Sprintf("**%s**: %s", t, strings.Join(models, ", ")))
		}
	}

	schemaProps := map[string]any{
		"file": map[string]any{
			"type":        "string",
			"format":      "binary",
			"description": "File to process",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Inference model. Required when multiple models are configured for the service type.\n\n" + strings.Join(typeParts, "\n\n"),
		},
		"callback_url": map[string]any{
			"type":        "string",
			"format":      "uri",
			"description": "Webhook URL called when the job completes",
			"example":     "https://your-app.example.com/webhook",
		},
	}
	if len(allOps) > 0 {
		opDesc := "Operation to perform. Required when the model has multiple operations."
		schemaProps["operation"] = map[string]any{
			"type":        "string",
			"enum":        allOps,
			"description": opDesc,
		}
	}

	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Submit an async inference job",
			"operationId": "submitJob",
			"parameters": []any{
				map[string]any{
					"name": "service_type", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": serviceTypes},
					"example": serviceTypes[0],
				},
			},
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   []string{"file"},
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"202": map[string]any{
					"description": "Job accepted",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobSubmitResponse"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

func getJobPathItem(serviceTypes []string) map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Get job status and result",
			"operationId": "getJob",
			"description": "Returns the current status of a job. When `completed`, the result is inlined.\n\n**The result file is deleted after this call** — subsequent calls return 404.",
			"parameters": []any{
				map[string]any{
					"name": "service_type", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": serviceTypes},
				},
				map[string]any{
					"name": "id", "in": "path", "required": true,
					"schema":  map[string]any{"type": "string", "format": "uuid"},
					"example": "550e8400-e29b-41d4-a716-446655440000",
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Job found",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobStatusResponse"},
						},
					},
				},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
			},
		},
	}
}

func listModelsPathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Inference"},
			"summary":     "List available models",
			"description": "Returns all configured models in OpenAI-compatible format.",
			"operationId": "listModels",
			"responses": map[string]any{
				"200": map[string]any{
					"description": "List of models",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ModelList"},
						},
					},
				},
			},
		},
	}
}

// syncPathItems generates one path item per unique URL path found across all
// service definitions. Pattern paths (containing {model}) get a path parameter.
func syncPathItems(reg *service.Registry) map[string]any {
	type entry struct {
		models       []string
		opNames      []string
		exts         []string
		hasSyncTopic bool
		isPattern    bool
	}

	byPath := map[string]*entry{}

	for _, def := range reg.All() {
		if def.InferenceURL == "" && def.SyncTopic == "" {
			continue
		}
		for opName, paths := range def.Operations {
			for _, p := range paths {
				if p == "" {
					continue
				}
				e := byPath[p]
				if e == nil {
					e = &entry{isPattern: strings.Contains(p, "{model}")}
					byPath[p] = e
				}
				e.models = appendUniq(e.models, def.Model)
				e.opNames = appendUniq(e.opNames, opName)
				for ext := range def.AcceptedExts {
					e.exts = appendUniq(e.exts, ext)
				}
				if def.SyncTopic != "" {
					e.hasSyncTopic = true
				}
			}
		}
	}

	items := map[string]any{}
	for p, e := range byPath {
		sort.Strings(e.models)
		sort.Strings(e.opNames)
		sort.Strings(e.exts)

		// Request body schema
		schemaProps := map[string]any{}
		required := []string{}

		if !e.isPattern {
			modelField := map[string]any{"type": "string"}
			if len(e.models) > 0 {
				modelField["enum"] = e.models
				modelField["example"] = e.models[0]
			}
			schemaProps["model"] = modelField
			if len(e.models) > 1 {
				required = append(required, "model")
			}
		}

		if len(e.opNames) > 1 {
			schemaProps["operation"] = map[string]any{
				"type":        "string",
				"enum":        e.opNames,
				"description": "Operation to perform. Required when the model supports multiple operations.",
			}
		}

		fileField := map[string]any{"type": "string", "format": "binary"}
		if len(e.exts) > 0 {
			fileField["description"] = "Accepted formats: " + strings.Join(e.exts, ", ")
		}
		schemaProps["file"] = fileField
		required = append(required, "file")

		// Summary from operation names
		summary := strings.Join(e.opNames, " / ")
		if summary != "" {
			summary = strings.ToUpper(summary[:1]) + summary[1:] + " (sync)"
		} else {
			summary = "Inference (sync)"
		}

		var desc string
		if e.hasSyncTopic {
			desc = "Sync-over-Kafka: processed via a priority Kafka topic; connection is held open until the result is ready."
		} else {
			desc = "Direct proxy to the inference backend (`inference_url`)."
		}

		op := map[string]any{
			"tags":        []string{"Inference"},
			"summary":     summary,
			"description": desc,
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   required,
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Inference result",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"type": "object"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"422": map[string]any{"$ref": "#/components/responses/UnprocessableEntity"},
				"504": map[string]any{"$ref": "#/components/responses/GatewayTimeout"},
				"502": map[string]any{"$ref": "#/components/responses/BadGateway"},
			},
		}

		if e.isPattern {
			op["parameters"] = []any{
				map[string]any{
					"name": "model", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": e.models},
				},
			}
		}

		items[p] = map[string]any{"post": op}
	}

	return items
}

// ── Shared components ──────────────────────────────────────────────────────────

func specComponents() map[string]any {
	return map[string]any{
		"schemas": map[string]any{
			"JobSubmitResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid", "example": "550e8400-e29b-41d4-a716-446655440000"},
					"service_type": map[string]any{"type": "string", "example": "audio"},
					"model":        map[string]any{"type": "string", "example": "whisper-large-v3"},
					"status":       map[string]any{"type": "string", "example": "pending"},
				},
			},
			"JobStatusResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid"},
					"service_type": map[string]any{"type": "string"},
					"model":        map[string]any{"type": "string"},
					"status":       map[string]any{"$ref": "#/components/schemas/JobStatus"},
					"result":       map[string]any{"type": "object", "description": "Inline result payload — present only when status is `completed`"},
					"error":        map[string]any{"type": "string", "description": "Error message — present only when status is `failed`"},
					"created_at":   map[string]any{"type": "string", "format": "date-time"},
					"updated_at":   map[string]any{"type": "string", "format": "date-time"},
				},
			},
			"JobStatus": map[string]any{
				"type":    "string",
				"enum":    []string{"pending", "processing", "completed", "failed"},
				"example": "completed",
			},
			"ModelList": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{"type": "string", "example": "list"},
					"data": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/Model"},
					},
				},
			},
			"Model": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string", "example": "whisper-large-v3"},
					"object":   map[string]any{"type": "string", "example": "model"},
					"owned_by": map[string]any{"type": "string", "example": "kevent"},
				},
			},
			"Error": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"error": map[string]any{"type": "string", "example": "field 'file' is required"},
				},
			},
		},
		"responses": map[string]any{
			"BadRequest": map[string]any{
				"description": "Invalid request",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"NotFound": map[string]any{
				"description": "Resource not found",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"UnprocessableEntity": map[string]any{
				"description": "Inference failed",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"InternalError": map[string]any{
				"description": "Internal server error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"GatewayTimeout": map[string]any{
				"description": "Timed out waiting for inference result",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"BadGateway": map[string]any{
				"description": "Upstream inference backend error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
		},
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func uniqueSorted(s []string) []string {
	seen := map[string]struct{}{}
	out := s[:0]
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

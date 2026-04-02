package handler

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"kevent/gateway/internal/config"
)

//go:embed testdata/whisper-api-openapi.json
var whisperFixture []byte

// serveFixture starts a test HTTP server returning the given body with the given status.
func serveFixture(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestFetchSwaggerSpecs_Success(t *testing.T) {
	srv := serveFixture(t, http.StatusOK, whisperFixture)
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}

	specs := FetchSwaggerSpecs(cfgs)

	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Type != "audio" || specs[0].Model != "whisper-large-v3" {
		t.Errorf("unexpected spec identity: %s/%s", specs[0].Type, specs[0].Model)
	}
	if !json.Valid(specs[0].Data) {
		t.Error("spec data is not valid JSON")
	}
}

func TestFetchSwaggerSpecs_NoURL(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3"}, // no SwaggerURL
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs for service without swagger_url, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_HTTP404(t *testing.T) {
	srv := serveFixture(t, http.StatusNotFound, []byte("not found"))
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs on 404, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_InvalidJSON(t *testing.T) {
	srv := serveFixture(t, http.StatusOK, []byte("not json at all"))
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs on invalid JSON, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_PartialFailure(t *testing.T) {
	good := serveFixture(t, http.StatusOK, whisperFixture)
	defer good.Close()
	bad := serveFixture(t, http.StatusInternalServerError, []byte("error"))
	defer bad.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: good.URL},
		{Type: "ocr", Model: "deepseek-ocr", SwaggerURL: bad.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec (partial failure), got %d", len(specs))
	}
	if specs[0].Type != "audio" {
		t.Errorf("expected audio spec, got %s", specs[0].Type)
	}
}

func TestNewSwaggerHandler_Found(t *testing.T) {
	specs := []SwaggerSpec{
		{Type: "audio", Model: "whisper-large-v3", Data: whisperFixture},
	}

	r := chi.NewRouter()
	r.Get("/swagger/{type}/{model}", NewSwaggerHandler(specs))

	req := httptest.NewRequest(http.MethodGet, "/swagger/audio/whisper-large-v3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Error("response body is not valid JSON")
	}
}

func TestNewSwaggerHandler_NotFound(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/swagger/{type}/{model}", NewSwaggerHandler(nil))

	req := httptest.NewRequest(http.MethodGet, "/swagger/audio/unknown-model", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDocsUI_ContainsSwaggerURLs(t *testing.T) {
	specs := []SwaggerSpec{
		{Type: "audio", Model: "whisper-large-v3", Data: whisperFixture},
	}

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	w := httptest.NewRecorder()
	DocsUI(specs)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !contains(body, "/swagger/audio/whisper-large-v3") {
		t.Error("expected swagger URL for whisper in docs HTML")
	}
	if !contains(body, "/openapi.yaml") {
		t.Error("expected gateway openapi.yaml URL in docs HTML")
	}
	if !contains(body, "audio / whisper-large-v3") {
		t.Error("expected service label in docs HTML")
	}
}

func TestDocsUI_NoSpecs(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	w := httptest.NewRecorder()
	DocsUI(nil)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !contains(w.Body.String(), "/openapi.yaml") {
		t.Error("expected gateway openapi.yaml URL even without service specs")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

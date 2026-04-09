package handler

import (
	"log/slog"
	"net/http"
)

// NewReloadHandler returns a handler for POST /-/reload.
// fn is called to reload the live configuration; errors are returned as 500.
func NewReloadHandler(fn func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("config reload requested", "remote", r.RemoteAddr)
		if err := fn(); err != nil {
			slog.Error("config reload failed", "error", err)
			writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
			return
		}
		slog.Info("config reloaded successfully")
		w.WriteHeader(http.StatusNoContent)
	}
}

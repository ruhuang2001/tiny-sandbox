package daemon

import (
	"encoding/json"
	"net/http"
)

// Route is one endpoint the server registers: a method and an OpenAPI-style
// path template, together with its handler.
type Route struct {
	Method string
	Path   string
	Handle http.HandlerFunc
}

// Routes returns the endpoints the server registers. Stage 0 freezes the
// contract: every operation in api/openapi.yaml has a route here, and a test
// fails if the two sets differ in either direction. Handler bodies are filled
// in by the stages that own them; until then they answer 503.
func Routes(version string) []Route {
	return []Route{
		{http.MethodGet, "/healthz", handleHealth(version)},

		{http.MethodPost, "/v1/sandboxes", notImplemented},
		{http.MethodGet, "/v1/sandboxes", notImplemented},
		{http.MethodGet, "/v1/sandboxes/{sandbox_id}", notImplemented},
		{http.MethodDelete, "/v1/sandboxes/{sandbox_id}", notImplemented},
		{http.MethodPost, "/v1/sandboxes/{sandbox_id}/pause", notImplemented},
		{http.MethodPost, "/v1/sandboxes/{sandbox_id}/resume", notImplemented},

		{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions", notImplemented},
		{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions", notImplemented},
		{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}", notImplemented},
		{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel", notImplemented},
		{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/events", notImplemented},

		{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files", notImplemented},
		{http.MethodPut, "/v1/sandboxes/{sandbox_id}/files", notImplemented},
	}
}

// NewHandler returns the server's HTTP handler.
func NewHandler(version string) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range Routes(version) {
		mux.HandleFunc(route.Method+" "+route.Path, route.Handle)
	}
	return mux
}

func handleHealth(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	}
}

// notImplemented answers routes whose handlers arrive in a later stage. 503 is
// the contract's "not ready" response, so clients retry rather than misread it.
func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusServiceUnavailable, "unavailable", "handler not implemented in this build")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

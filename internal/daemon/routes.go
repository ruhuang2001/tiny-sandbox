package daemon

import (
	"encoding/json"
	"net/http"
)

// Operation is one endpoint of the HTTP contract: a method and an
// OpenAPI-style path template.
type Operation struct {
	Method string
	Path   string
}

// routes is the single list of contract operations, in registration order.
// The mux, the handler switch, and the contract tests all work from it, so an
// operation cannot be added in one place and forgotten in another.
var routes = []Operation{
	{http.MethodGet, "/healthz"},

	{http.MethodPost, "/v1/sandboxes"},
	{http.MethodGet, "/v1/sandboxes"},
	{http.MethodGet, "/v1/sandboxes/{sandbox_id}"},
	{http.MethodDelete, "/v1/sandboxes/{sandbox_id}"},
	{http.MethodPost, "/v1/sandboxes/{sandbox_id}/pause"},
	{http.MethodPost, "/v1/sandboxes/{sandbox_id}/resume"},

	{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions"},
	{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions"},
	{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}"},
	{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel"},
	{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/events"},

	{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files"},
	{http.MethodPut, "/v1/sandboxes/{sandbox_id}/files"},
}

// Routes returns the operations the server registers. Stage 0 freezes the
// contract: every operation in api/openapi.yaml has an entry here, and a test
// fails if the two sets differ in either direction.
func Routes() []Operation {
	return append([]Operation(nil), routes...)
}

// handleHealth is the liveness endpoint. It answers without touching the
// container runtime: a liveness probe must not restart the server because
// Docker is briefly unreachable.
func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// routeKey names one operation. It is the string the mux registers and the
// string handlerFor matches.
func routeKey(method, path string) string {
	return method + " " + path
}

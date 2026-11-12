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

// routeHandler is the shape of every Handler method that serves a route: a
// method expression such as (*Handler).createSandbox. It is not http.HandlerFunc
// because the table stores unbound methods, so the same table serves any
// Handler instance.
type routeHandler func(*Handler, http.ResponseWriter, *http.Request)

// Route is an Operation bound to the handler that serves it. Keeping the two
// together is what makes the table the single source of truth: adding an
// operation means adding one entry, and there is no second list to forget.
type Route struct {
	Operation
	handle routeHandler
}

// routes is the single list of contract operations, in registration order.
// The mux, the handler lookup, and the contract tests all work from it.
var routes = []Route{
	{Operation{http.MethodGet, "/healthz"}, (*Handler).handleHealth},

	{Operation{http.MethodPost, "/v1/sandboxes"}, (*Handler).createSandbox},
	{Operation{http.MethodGet, "/v1/sandboxes"}, (*Handler).listSandboxes},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}"}, (*Handler).getSandbox},
	{Operation{http.MethodDelete, "/v1/sandboxes/{sandbox_id}"}, (*Handler).deleteSandbox},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/pause"}, (*Handler).pauseSandbox},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/resume"}, (*Handler).resumeSandbox},

	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions"}, (*Handler).startExecution},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions"}, (*Handler).listExecutions},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}"}, (*Handler).getExecution},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel"}, (*Handler).cancelExecution},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/events"}, (*Handler).streamEvents},

	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files"}, (*Handler).readFile},
	{Operation{http.MethodPut, "/v1/sandboxes/{sandbox_id}/files"}, (*Handler).writeFile},
}

// Routes returns the operations the server registers. Stage 0 freezes the
// contract: every operation in api/openapi.yaml has an entry here, and a test
// fails if the two sets differ in either direction.
func Routes() []Operation {
	ops := make([]Operation, len(routes))
	for i, route := range routes {
		ops[i] = route.Operation
	}
	return ops
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
// string a lookup uses.
func routeKey(method, path string) string {
	return method + " " + path
}

package daemon

import (
	"net/http"
	"testing"
)

// TestEveryOperationResolvesToAHandler is the guard the route coverage test
// cannot provide: a mux built with a fallback handler reports a matching
// pattern for every listed route, so only resolving the table directly proves
// each operation has a real handler.
func TestEveryOperationResolvesToAHandler(t *testing.T) {
	h := NewHandler(nil, "test-version")
	for _, op := range routes {
		handle, ok := h.handlerFor(op.Method, op.Path)
		if !ok || handle == nil {
			t.Errorf("%s has no handler", routeKey(op.Method, op.Path))
		}
	}
}

// TestUnknownOperationHasNoHandler pins the negative side: the lookup itself
// must report a miss rather than hand back some default, so the panic in
// newMux is reachable.
func TestUnknownOperationHasNoHandler(t *testing.T) {
	h := NewHandler(nil, "test-version")
	if handle, ok := h.handlerFor(http.MethodGet, "/v1/nope"); ok || handle != nil {
		t.Fatalf("unknown operation resolved to %v, want a miss", handle)
	}
}

// TestMissingHandlerPanicsAtBuildTime pins the failure mode for a route added
// to the table without a case in handlerFor: construction panics, so the
// server never starts with an operation it cannot serve. The old behaviour
// answered 503 on the first request instead, which no test observed.
func TestMissingHandlerPanicsAtBuildTime(t *testing.T) {
	h := NewHandler(nil, "test-version")
	defer func() {
		if recover() == nil {
			t.Fatal("buildMux accepted an operation with no handler; want a panic")
		}
	}()
	newMux(h, []Operation{{http.MethodGet, "/v1/not-implemented"}})
}

// TestRouteTableHasNoDuplicates keeps the mux registration honest: a
// duplicated method and path panics inside http.ServeMux, and a duplicated
// pair with different handlers would silently keep one of them.
func TestRouteTableHasNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(routes))
	for _, op := range routes {
		key := routeKey(op.Method, op.Path)
		if seen[key] {
			t.Errorf("route %s is listed twice", key)
		}
		seen[key] = true
	}
}

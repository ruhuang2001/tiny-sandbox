package daemon

import (
	"net/http"
	"testing"
)

// TestEveryRouteHasAHandler is the guard the route coverage test cannot
// provide: a mux with a fallback handler reports a matching pattern for every
// listed route, so only inspecting the table proves each entry is servable.
func TestEveryRouteHasAHandler(t *testing.T) {
	for _, route := range routes {
		key := routeKey(route.Method, route.Path)
		if route.handle == nil {
			t.Errorf("%s has no handler", key)
			continue
		}
		if route.handle == nil {
			t.Errorf("%s bound to a nil handler", key)
		}
	}
}

// TestMissingHandlerPanicsAtBuildTime pins the failure mode for an entry added
// to a table without a handler: construction panics, so the server never
// starts with an operation it cannot serve. The old behaviour answered 503 on
// the first request instead, which no test observed.
func TestMissingHandlerPanicsAtBuildTime(t *testing.T) {
	h := NewHandler(nil, "test-version")
	defer func() {
		if recover() == nil {
			t.Fatal("newMux accepted a route with no handler; want a panic")
		}
	}()
	newMux(h, []Route{{Operation: Operation{Method: http.MethodGet, Path: "/v1/not-implemented"}}})
}

// TestRouteTableHasNoDuplicates keeps the mux registration honest: a
// duplicated method and path panics inside http.ServeMux, and a duplicated
// pair with different handlers would silently keep one of them.
func TestRouteTableHasNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := routeKey(route.Method, route.Path)
		if seen[key] {
			t.Errorf("route %s is listed twice", key)
		}
		seen[key] = true
	}
}

// TestRoutesReturnsACopy keeps callers from mutating the table through the
// exported accessor.
func TestRoutesReturnsACopy(t *testing.T) {
	first := Routes()
	if len(first) == 0 {
		t.Fatal("route table is empty")
	}
	want := first[0]
	first[0] = Operation{Method: http.MethodGet, Path: "/v1/mutated"}

	if got := Routes()[0]; got != want {
		t.Fatalf("Routes() returned the table itself: first entry is %v, want %v", got, want)
	}
}

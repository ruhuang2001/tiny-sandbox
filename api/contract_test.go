package api_test

import (
	"context"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/api"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
	"gopkg.in/yaml.v3"
)

func TestSpecFieldsMatchRequestAndSandboxTypes(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec(), &doc); err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}
	for _, tc := range []struct {
		schema string
		value  any
	}{
		{"CreateSandboxRequest", daemon.CreateRequest{}},
		{"CreateSandboxRequest", sandbox.CreateSandboxRequest{}},
		{"ExecuteRequest", daemon.ExecuteRequest{}},
		{"ExecuteRequest", sandbox.ExecuteRequest{}},
		{"Sandbox", daemon.Sandbox{}},
		{"Sandbox", sandbox.Sandbox{}},
	} {
		typ := reflect.TypeOf(tc.value)
		t.Run(tc.schema+"/"+typ.PkgPath(), func(t *testing.T) {
			properties := doc.Components.Schemas[tc.schema].Properties
			fields := make(map[string]bool)
			for i := range typ.NumField() {
				name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				fields[name] = true
				if _, ok := properties[name]; !ok {
					t.Errorf("OpenAPI %s is missing field %s", tc.schema, name)
				}
			}
			for name := range properties {
				if !fields[name] {
					t.Errorf("%s is missing OpenAPI field %s", typ, name)
				}
			}
		})
	}
}

// TestSpecMatchesAPIMD checks that api/openapi.yaml and docs/api.md describe
// the same operations. api.md is the prose contract; the spec is its
// machine-readable form, and neither may drift from the other.
func TestSpecMatchesAPIMD(t *testing.T) {
	want := operationsInAPIMD(t)
	got, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}

	missing, extra := api.Diff(want, got)
	for _, op := range missing {
		t.Errorf("docs/api.md documents %s but api/openapi.yaml does not declare it", op)
	}
	for _, op := range extra {
		t.Errorf("api/openapi.yaml declares %s but docs/api.md does not document it", op)
	}
}

// endpointHeading matches "### `GET /healthz`" and the two-operation form
// "### `POST /v1/.../pause`, `POST /v1/.../resume`".
var endpointHeading = regexp.MustCompile("`(GET|POST|PUT|DELETE|PATCH) ([^`]+)`")

// operationsInAPIMD extracts the endpoint set from the "Endpoints" section
// headings of api.md.
func operationsInAPIMD(t *testing.T) []api.Operation {
	t.Helper()
	data, err := os.ReadFile("../docs/api.md")
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}

	var ops []api.Operation
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		for _, m := range endpointHeading.FindAllStringSubmatch(line, -1) {
			path := m[2]
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			ops = append(ops, api.Operation{Method: m[1], Path: path})
		}
	}
	if len(ops) == 0 {
		t.Fatal("docs/api.md documents no endpoints; the Endpoints headings changed format")
	}
	return ops
}

// TestServerRoutesMatchSpec checks the server's route table against the spec
// in both directions: every operation in the spec is registered, and every
// registered route is declared. A route that is declared but unreachable
// through the server's own mux also fails.
func TestServerRoutesMatchSpec(t *testing.T) {
	specOps, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}

	routes := daemon.Routes("test-version")
	served := make([]api.Operation, 0, len(routes))
	be := membackend.New()
	mux := daemon.NewHandler(daemon.New(be), "test-version")

	for _, route := range routes {
		op := api.Operation{Method: route.Method, Path: route.Path}
		served = append(served, op)

		req := httptest.NewRequestWithContext(context.Background(), route.Method, concretePath(route.Path), nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("route %s is listed in Routes but not reachable through the server mux", op)
		}
	}

	missing, extra := api.Diff(specOps, served)
	for _, op := range missing {
		t.Errorf("spec declares %s but the server registers no route for it", op)
	}
	for _, op := range extra {
		t.Errorf("server registers %s but the spec does not declare it", op)
	}
}

// concretePath substitutes placeholder segments so a route can be looked up
// in the mux.
func concretePath(path string) string {
	return strings.NewReplacer(
		"{sandbox_id}", "sbx_test",
		"{execution_id}", "exe_test",
	).Replace(path)
}

// TestDiffReportsBothDirections pins the comparison itself: a missing
// operation and an extra one are reported independently, so a route table that
// only partly matches cannot pass.
func TestDiffReportsBothDirections(t *testing.T) {
	want := []api.Operation{
		{Method: "GET", Path: "/a"},
		{Method: "POST", Path: "/a"},
	}
	got := []api.Operation{
		{Method: "GET", Path: "/a"},
		{Method: "DELETE", Path: "/b"},
	}

	missing, extra := api.Diff(want, got)
	if len(missing) != 1 || missing[0].String() != "POST /a" {
		t.Fatalf("missing = %v, want [POST /a]", missing)
	}
	if len(extra) != 1 || extra[0].String() != "DELETE /b" {
		t.Fatalf("extra = %v, want [DELETE /b]", extra)
	}
}

// TestOperationsCoversEveryDocumentedEndpoint is a guard on the parser: the
// spec has one operation per endpoint heading in api.md.
func TestOperationsCoversEveryDocumentedEndpoint(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}
	if len(ops) != 14 {
		t.Fatalf("parsed %d operations, want 14: %v", len(ops), ops)
	}
}

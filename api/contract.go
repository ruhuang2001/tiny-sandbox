// Package api embeds the HTTP contract and exposes the operation set the
// server's routes are checked against.
//
// The embedded document is the same file docs/api.md freezes: api.md is the
// prose contract, openapi.yaml its machine-readable form. A test in this
// package compares the operation set parsed from the spec with the routes the
// server registers, so the two cannot drift silently.
package api

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var spec []byte

// httpMethods is the set of keys under a path item that denote operations.
// Everything else (parameters, summary, description, servers) is ignored.
var httpMethods = map[string]bool{
	"get":     true,
	"put":     true,
	"post":    true,
	"delete":  true,
	"patch":   true,
	"head":    true,
	"options": true,
	"trace":   true,
}

// Spec returns the embedded OpenAPI document.
func Spec() []byte {
	return spec
}

// Operation is one method and path template in the contract.
type Operation struct {
	Method string // uppercase HTTP method, for example "GET"
	Path   string // OpenAPI path template, for example "/v1/sandboxes/{sandbox_id}"
}

// String renders the operation as "GET /v1/sandboxes/{sandbox_id}".
func (o Operation) String() string {
	return o.Method + " " + o.Path
}

// Operations parses the embedded spec and returns its operations, sorted by
// path and then method.
func Operations() ([]Operation, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("api: parse embedded spec: %w", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("api: embedded spec defines no paths")
	}

	ops := make([]Operation, 0, len(doc.Paths))
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("api: spec path %q is not absolute", path)
		}
		for key := range item {
			method := strings.ToLower(key)
			if !httpMethods[method] {
				continue
			}
			ops = append(ops, Operation{Method: strings.ToUpper(method), Path: path})
		}
	}
	sortOperations(ops)
	return ops, nil
}

// Diff compares want with got and reports the operations present in only one
// of them. Both results are sorted.
func Diff(want, got []Operation) (missing, extra []Operation) {
	index := make(map[Operation]bool, len(got))
	for _, op := range got {
		index[op] = true
	}
	for _, op := range want {
		if !index[op] {
			missing = append(missing, op)
		}
		delete(index, op)
	}
	for op := range index {
		extra = append(extra, op)
	}
	sortOperations(missing)
	sortOperations(extra)
	return missing, extra
}

func sortOperations(ops []Operation) {
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
}

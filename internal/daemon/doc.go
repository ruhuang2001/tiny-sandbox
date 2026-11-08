// Package daemon owns the sandbox state machine, the execution engine, and
// termination classification. It depends only on a Backend interface, so the
// in-memory backend tests the semantics without Docker.
//
// Implementation begins with the server core stage described in
// docs/design.md.
package daemon

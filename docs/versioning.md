# Versioning and release

This repository is an interview demo, not a published hosted service. The Go module, `sandboxctl`, and `sandboxd` share one version number, and GoReleaser publishes nothing.

## Version numbers

Local tags follow semantic versioning: `MAJOR.MINOR.PATCH`.

- **patch** fixes behaviour without changing the public contract;
- **minor** adds a backward-compatible operation, field, or flag;
- **major** may remove or reinterpret public API.

Before `v1.0.0`, breaking changes are allowed but must be called out explicitly in the change description.

## What counts as the contract

The compatibility surface is wider than the Go API:

- CLI flags, environment variables, and exit codes;
- JSON request and response fields;
- SDK error types and the sentinel values `errors.Is` matches;
- timeout units and their meaning;
- SSE event types, event ID semantics, and `Last-Event-ID` behaviour.

`api/openapi.yaml` is the single source for HTTP behaviour. The embedded copy in `api/contract.go` and the server's routes are checked against each other by a test, so the spec cannot drift silently.

## Additive changes

Prefer a new optional field or a new operation over changing an existing one. The server ignores unknown request fields, so a newer client can talk to an older server; the old server simply does not act on the new field. The client must therefore not assume a field took effect, and must document which contract version it needs. Old clients keep working against a new server because responses only gain fields.

Removing a field, changing a unit, or reinterpreting an existing value is a major change.

## Release checklist

```sh
go build ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
goreleaser check
goreleaser release --snapshot --clean
E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
go test -race -covermode=atomic -coverpkg=./... ./... \
  -args -test.gocoverdir="$PWD/artifacts/cov-unit"
bash scripts/coverage.sh artifacts/cov-unit artifacts/e2e/coverage 70
```

The last three commands are the same ones CI runs; `coverage.sh` merges both
profiles and fails below the gate. The e2e step needs a Docker daemon and no
other setup.

Inspect the archives in `dist/` and verify the checksum file. Do not upload anything or create a GitHub release.

## Rollback

Rolling back means checking out the previous tag and its binaries. It does not roll back sandbox state, and it does not undo commands that already ran. Gradual rollout, canarying, and automated rollback need production telemetry that this demo does not have.

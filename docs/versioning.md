# Versioning and release

The Go module, `sandboxctl`, and `sandboxd` share one version number. CI builds local GoReleaser snapshots and uploads them as workflow artifacts; no tag-triggered publication workflow is configured.

## Version numbers

Release tags use `vMAJOR.MINOR.PATCH`.

The OpenAPI `info.version` follows the release version. The `/v1` HTTP path
prefix is the API namespace, not a claim that the project has reached v1.0.0.

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

[api.md](api.md) is the prose HTTP contract and [api/openapi.yaml](../api/openapi.yaml) is its machine-readable form, embedded by `api/contract.go`. Existing checks compare method/path sets across the prose, specification, and router; they do not prove that every schema or behavioral detail matches. Review those details when changing the contract.

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
mkdir -p artifacts/cov-unit
go test -race -covermode=atomic -coverpkg=./... ./... \
  -args -test.gocoverdir="$PWD/artifacts/cov-unit"
bash scripts/coverage.sh artifacts/cov-unit artifacts/e2e/coverage 70
```

The coverage steps follow CI: `coverage.sh` merges both profiles and enforces
the 70% gate. Use fresh artifact directories for each run. The E2E suite needs
a reachable Docker daemon, the Docker CLI and curl; missing container
images also require registry access. The script starts its own server on port
18081 by default; set `E2E_PORT` if that port is occupied.

Inspect the six platform archives in `dist/` and verify `checksums.txt`
(`sha256sum -c checksums.txt`, or `shasum -a 256 -c checksums.txt` on macOS,
from that directory). `--snapshot` creates local artifacts without publishing.
Publishing a release is a separate action from this verification checklist.

## Publish manually

After the checklist passes, use a clean checkout of the intended release
commit. The example below publishes `v0.0.1`; change the version consistently
for later releases.

```sh
git tag -a v0.0.1 -m 'v0.0.1'
goreleaser release --clean --skip=publish
(cd dist && shasum -a 256 -c checksums.txt)
git push origin v0.0.1
gh release create v0.0.1 --verify-tag --title v0.0.1 \
  --notes-file /path/to/release-notes.md \
  dist/tiny-sandbox_0.0.1_*.tar.gz \
  dist/tiny-sandbox_0.0.1_*.zip dist/checksums.txt
```

The build command creates versioned release artifacts, not snapshots.
Publishing requires GitHub authentication and write access; pushing a tag alone
does not publish binaries. Write the release notes before running the final
command. After publication, download the six archives and `checksums.txt` from
the Release, verify all hashes, and check the native binaries' version output
(`sandboxctl version` and `sandboxd -version`).

## Rollback

Rolling back means checking out the previous tag and its binaries. It does not roll back sandbox state, and it does not undo commands that already ran. Server state is not persisted across restarts. Deployment orchestration,
canarying, and automated rollback are outside this repository's scope.

## Configuration

The [CI workflow](../.github/workflows/ci.yml), [GoReleaser configuration](../.goreleaser.yaml),
[E2E script](../scripts/e2e.sh), and [coverage merger](../scripts/coverage.sh)
are the executable sources for the checks described here.

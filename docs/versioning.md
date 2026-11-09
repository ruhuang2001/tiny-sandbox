# Versioning and release

The Go module, `sandboxctl`, and `sandboxd` share one version number. Ordinary
CI builds GoReleaser snapshots as workflow artifacts. The separate Release
workflow validates a release tag through the same CI before building and
publishing versioned binaries.

The TypeScript SDK in sandbox-ts has an independent npm package version. This
repository currently builds and validates its tarball without publishing it.
Its [compatibility contract](sdk-typescript.md#compatibility-and-go-parity)
includes exported types, codes, outcome discriminants, retry defaults and
iterator cleanup. Wire additions must remain usable by existing consumers.

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
make typescript
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
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

## Publish through GitHub Actions

After preparing the intended release commit on `main`, update OpenAPI's
`info.version`, create a stable `vMAJOR.MINOR.PATCH` tag, and push it. Tags
containing the Release workflow trigger publication automatically:

```sh
git tag -a v0.0.2 -m 'v0.0.2'
git push origin v0.0.2
```

For an existing tag, including one created before the workflow was added,
dispatch from `main` without moving the tag:

```sh
gh workflow run release.yml --ref main -f tag=v0.0.1
```

The workflow resolves the tag to an immutable commit on `main`, runs all five
CI jobs against that commit, and only then grants the publishing job
`contents: write`. It builds six non-snapshot archives, verifies their
checksums and native version output, uploads to a draft, downloads the
attachments again, and checks them against the CI build before publishing.
The release notes include a link to the responsible Actions run.

Existing release notes are retained. A published Release is never overwritten.
If a failed or earlier manual attempt left same-name assets in a draft,
inspect and back them up before explicitly allowing replacement:

```sh
gh workflow run release.yml --ref main \
  -f tag=v0.0.1 -f replace_draft_assets=true
```

That option deletes and replaces same-name draft attachments; it does not
delete the draft or move the tag. It defaults to false. The CI-built archives
and checksums are also retained as an Actions artifact for seven days.

After publication, download the six archives and `checksums.txt` from the
Release, verify all hashes, and check the native binaries' version output
(`sandboxctl version` and `sandboxd -version`). Do not upload local binaries
as a substitute for this workflow.

## Rollback

Rolling back means checking out the previous tag and its binaries. It does not roll back sandbox state, and it does not undo commands that already ran. Server state is not persisted across restarts. Deployment orchestration,
canarying, and automated rollback are outside this repository's scope.

## Configuration

The [CI workflow](../.github/workflows/ci.yml), [Release workflow](../.github/workflows/release.yml),
[GoReleaser configuration](../.goreleaser.yaml),
[E2E script](../scripts/e2e.sh), and [coverage merger](../scripts/coverage.sh)
are the executable sources for the checks described here.
